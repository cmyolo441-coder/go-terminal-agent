package collab

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Error is a request failure with an HTTP status and a stable code.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

var (
	errUnauthorized = &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "invalid participant token"}
	errClosed       = &Error{Status: http.StatusGone, Code: "room_closed", Message: "the room is closed"}
)

// participant is actor-owned state for one member.
type participant struct {
	id, name    string
	role        Role
	tokenHash   [32]byte
	joined      time.Time
	lastSeen    time.Time
	online      bool
	status      string
	cursor      *int
	sel         *Selection
	typingUntil time.Time
	streams     int
	bucket      tokenBucket
	lastSeq     uint64
	stale       map[string]bool // documents this replica must resync
	offlineAt   time.Time
}

func (p *participant) info() ParticipantInfo {
	return ParticipantInfo{
		ID: p.id, Name: p.name, Role: p.role, Online: p.online, Status: p.status,
		Cursor: p.cursor, Sel: p.sel, Typing: !p.typingUntil.IsZero(), LastSeen: p.lastSeen, Joined: p.joined,
	}
}

// subscriber is one open event stream.
type subscriber struct {
	p      *participant
	ch     chan []byte
	done   chan struct{} // closed by the actor when it drops the stream
	reason DisconnectEvent
}

// RoomStats are operational counters.
type RoomStats struct {
	Seq              uint64    `json:"seq"`
	Participants     int       `json:"participants"`
	Online           int       `json:"online"`
	Streams          int       `json:"streams"`
	SlowDisconnects  uint64    `json:"slow_disconnects"`
	DroppedEphemeral uint64    `json:"dropped_ephemeral"`
	RateLimited      uint64    `json:"rate_limited"`
	Rejected         uint64    `json:"rejected"`
	PendingApprovals int       `json:"pending_approvals"`
	Prompt           TextStats `json:"prompt"`
	LastGC           GCStats   `json:"last_gc"`
	RecordErrors     uint64    `json:"record_errors"`
}

// Room is one collaboration session. All of its state is owned by a single
// goroutine (the actor); every method sends a closure to it, so there are
// no locks on the hot path and events are totally ordered.
type Room struct {
	id      string
	title   string
	hub     *Hub
	opts    *Options
	log     *slog.Logger
	created time.Time
	policy  QuorumPolicy

	cmds      chan func()
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeMu   sync.Mutex
	closeWhy  string

	// Actor-owned.
	seq          uint64
	ring         *ringLog
	participants map[string]*participant
	subs         map[*subscriber]struct{}
	chat         []ChatMessage
	approvals    map[string]*approval
	prompt       *Text
	context      *ORSet
	settings     *LWWMap
	reports      map[string]map[string]VersionVector // doc -> replica -> report
	recorder     Recorder
	lastGC       time.Time
	idleSince    time.Time
	stats        RoomStats
}

func newRoom(h *Hub, id, title string, policy QuorumPolicy, rec Recorder) *Room {
	o := &h.opts
	now := o.Now()
	clock := NewClock(ServerReplica, WithPhysicalClock(o.Now), WithMaxDrift(o.MaxDrift))
	r := &Room{
		id: id, title: title, hub: h, opts: o, log: o.Logger.With("room", id), created: now.UTC(), policy: policy,
		cmds: make(chan func(), DefaultInboxSize), stop: make(chan struct{}), done: make(chan struct{}),
		ring: newRingLog(o.ReplayBuffer), participants: map[string]*participant{}, subs: map[*subscriber]struct{}{},
		approvals: map[string]*approval{},
		prompt:    NewText(ServerReplica), context: NewORSet(ServerReplica), settings: NewLWWMap(ServerReplica, clock),
		reports:  map[string]map[string]VersionVector{DocPrompt: {}, DocContext: {}, DocSettings: {}},
		recorder: rec, lastGC: now, idleSince: now,
	}
	return r
}

// ID returns the room id.
func (r *Room) ID() string { return r.id }

// Title returns the room title.
func (r *Room) Title() string { return r.title }

// Done is closed once the room has stopped.
func (r *Room) Done() <-chan struct{} { return r.done }

// Close stops the room, disconnecting everyone with the given reason.
func (r *Room) Close(reason string) {
	r.closeOnce.Do(func() {
		r.closeMu.Lock()
		r.closeWhy = reason
		r.closeMu.Unlock()
		close(r.stop)
	})
}

func (r *Room) run() {
	ticker := time.NewTicker(r.opts.TickInterval)
	defer ticker.Stop()
	defer close(r.done)
	for {
		select {
		case fn := <-r.cmds:
			r.safely(fn)
		case <-ticker.C:
			r.safely(func() { r.tick(r.now()) })
		case <-r.stop:
			r.shutdown()
			return
		}
	}
}

func (r *Room) safely(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			r.log.Error("collab room actor panic", "panic", v, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// exec runs fn on the actor and waits for it.
func (r *Room) exec(ctx context.Context, fn func()) error {
	finished := make(chan struct{})
	wrapped := func() {
		defer close(finished)
		fn()
	}
	select {
	case r.cmds <- wrapped:
	case <-r.done:
		return ErrRoomClosed
	case <-r.stop:
		return ErrRoomClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-finished:
		return nil
	case <-r.done:
		select {
		case <-finished:
			return nil
		default:
			return ErrRoomClosed
		}
	}
}

func (r *Room) now() time.Time { return r.opts.Now().UTC() }

func (r *Room) shutdown() {
	r.closeMu.Lock()
	why := r.closeWhy
	r.closeMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(r.approvals)) {
		a := r.approvals[id]
		delete(r.approvals, id)
		o := a.resolve(DecisionCancelled, "room closed")
		r.emit(EvApprovalResolved, "", o, false)
	}
	r.emit(EvClosed, "", DisconnectEvent{Reason: ReasonRoomClosed, Detail: why}, false)
	for s := range r.subs {
		r.dropSub(s, DisconnectEvent{Reason: ReasonRoomClosed, Detail: why})
	}
	if r.recorder != nil {
		if err := r.recorder.Close(); err != nil {
			r.log.Warn("collab recorder close failed", "err", err)
		}
	}
	r.log.Info("collab room closed", "reason", why)
}

// encodeFrame renders ev as one SSE event. JSON never contains a raw
// newline, so the payload is a single data line.
func encodeFrame(ev Event) []byte {
	data := mustJSON(ev)
	b := make([]byte, 0, len(data)+len(ev.Type)+40)
	if ev.Seq != 0 {
		b = append(b, "id: "...)
		b = strconv.AppendUint(b, ev.Seq, 10)
		b = append(b, '\n')
	}
	b = append(b, "event: "...)
	b = append(b, ev.Type...)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	return append(b, '\n', '\n')
}

// emit publishes an event to every stream and the recorder. Sequenced
// events are numbered, kept for resume, and never silently lost: a stream
// that cannot take one is disconnected so it resumes. Ephemeral events
// (presence) are best-effort.
func (r *Room) emit(typ, from string, data any, ephemeral bool) uint64 {
	ev := Event{Type: typ, Time: r.now(), From: from, Data: mustJSON(data)}
	if !ephemeral {
		r.seq++
		ev.Seq = r.seq
	}
	frame := encodeFrame(ev)
	if !ephemeral {
		r.ring.append(ev.Seq, frame)
	}
	if r.recorder != nil {
		if err := r.recorder.Record(ev); err != nil {
			r.stats.RecordErrors++
			if r.stats.RecordErrors == 1 {
				r.log.Error("collab recording failed", "err", err)
			}
		}
	}
	for s := range r.subs {
		select {
		case s.ch <- frame:
		default:
			if ephemeral {
				r.stats.DroppedEphemeral++
				continue
			}
			r.stats.SlowDisconnects++
			r.dropSub(s, DisconnectEvent{Reason: ReasonSlowConsumer, Detail: fmt.Sprintf("stream fell %d events behind; resume with Last-Event-ID", cap(s.ch))})
		}
	}
	return ev.Seq
}

func (r *Room) dropSub(s *subscriber, why DisconnectEvent) {
	if _, ok := r.subs[s]; !ok {
		return
	}
	delete(r.subs, s)
	s.reason = why
	close(s.done)
	s.p.streams--
	s.p.lastSeen = r.now()
}

func (r *Room) addParticipant(name string, role Role) (Credentials, error) {
	if len(r.participants) >= r.opts.MaxParticipants {
		return Credentials{}, errf(http.StatusConflict, "room_full", "the room has %d participants", len(r.participants))
	}
	token := newToken()
	now := r.now()
	p := &participant{
		id: "p_" + randomHex(6), name: name, role: role, tokenHash: sha256.Sum256([]byte(token)),
		joined: now, status: "offline", stale: map[string]bool{},
		bucket: newTokenBucket(r.opts.RateLimit, r.opts.RateBurst, now),
	}
	r.participants[p.id] = p
	return Credentials{RoomID: r.id, ParticipantID: p.id, Token: token, Role: role}, nil
}

// authenticate finds the participant holding token. It compares against
// every participant without stopping early, so timing reveals neither
// whether nor where a match occurred.
func (r *Room) authenticate(token string) *participant {
	if token == "" {
		return nil
	}
	h := sha256.Sum256([]byte(token))
	var found *participant
	for _, p := range r.participants {
		if subtle.ConstantTimeCompare(h[:], p.tokenHash[:]) == 1 {
			found = p
		}
	}
	return found
}

func (r *Room) setOnline(p *participant, now time.Time) {
	p.lastSeen = now
	if p.online {
		if p.status == "idle" || p.status == "offline" {
			p.status = "active"
			r.emit(EvPresence, p.id, p.info(), true)
		}
		return
	}
	p.online, p.status, p.offlineAt = true, "active", time.Time{}
	r.emit(EvJoined, p.id, p.info(), false)
}

func (r *Room) setOffline(p *participant, reason string, now time.Time) {
	if !p.online {
		return
	}
	p.online, p.status, p.typingUntil, p.offlineAt = false, "offline", time.Time{}, now
	r.emit(EvLeft, p.id, LeftEvent{ID: p.id, Reason: reason}, false)
}

func (r *Room) info() RoomInfo {
	online := 0
	for _, p := range r.participants {
		if p.online {
			online++
		}
	}
	return RoomInfo{ID: r.id, Title: r.title, Created: r.created, Participants: len(r.participants), Online: online, Seq: r.seq, Policy: r.policy}
}

func (r *Room) participantList() []ParticipantInfo {
	out := make([]ParticipantInfo, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p.info())
	}
	slices.SortFunc(out, func(a, b ParticipantInfo) int {
		if c := a.Joined.Compare(b.Joined); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return out
}

// subscribe opens a stream. With resume set, the backlog holds every
// sequenced event after lastID when they are still buffered.
func (r *Room) subscribe(ctx context.Context, token string, lastID uint64, resume bool) (*subscriber, [][]byte, *Error) {
	var (
		sub     *subscriber
		backlog [][]byte
		apiErr  *Error
	)
	err := r.exec(ctx, func() {
		p := r.authenticate(token)
		if p == nil {
			apiErr = errUnauthorized
			return
		}
		if p.streams >= r.opts.MaxStreamsPerParticipant {
			apiErr = errf(http.StatusTooManyRequests, ReasonReplaced, "at most %d concurrent streams per participant", r.opts.MaxStreamsPerParticipant)
			return
		}
		now := r.now()
		p.streams++
		r.setOnline(p, now)
		w := Welcome{Room: r.info(), You: p.info(), Seq: r.seq, Participants: r.participantList()}
		if resume {
			if frames, ok := r.ring.since(lastID); ok {
				w.Resumed, backlog = true, frames
			}
		}
		if !w.Resumed {
			w.Chat = slices.Clone(r.chat)
			for _, id := range slices.Sorted(maps.Keys(r.approvals)) {
				w.Approvals = append(w.Approvals, r.approvals[id].view)
			}
			ps, cs, ss := r.prompt.Snapshot(false), r.context.Snapshot(false), r.settings.Snapshot(false)
			w.Prompt, w.Context, w.Settings = &ps, &cs, &ss
			if p.role.AtLeast(RoleEditor) {
				// A client that starts from these snapshots has seen at
				// least this much; it rebases any unsent edits on top.
				r.report(p, DocPrompt, ps.VV, true)
				r.report(p, DocContext, cs.VV, true)
				r.report(p, DocSettings, ss.VV, true)
				for doc := range p.stale {
					delete(p.stale, doc)
				}
			}
		}
		welcome := encodeFrame(Event{Type: EvWelcome, Time: now, Data: mustJSON(w)})
		backlog = append([][]byte{welcome}, backlog...)
		sub = &subscriber{p: p, ch: make(chan []byte, r.opts.SubscriberBuffer), done: make(chan struct{})}
		r.subs[sub] = struct{}{}
	})
	if err != nil {
		return nil, nil, errClosed
	}
	return sub, backlog, apiErr
}

// unsubscribe forgets a stream whose client went away.
func (r *Room) unsubscribe(s *subscriber) {
	select {
	case r.cmds <- func() { r.dropSub(s, DisconnectEvent{Reason: ReasonLeft}) }:
	case <-r.done:
	case <-r.stop:
	}
}

// report records a replica's acknowledged version vector for doc. Only the
// replica's own component may exceed what the server holds (its unsent
// operations); the rest is clamped to the server's state.
func (r *Room) report(p *participant, doc string, vv VersionVector, replace bool) {
	if vv == nil || !p.role.AtLeast(RoleEditor) {
		return
	}
	var have VersionVector
	switch doc {
	case DocPrompt:
		have = r.prompt.log.vv
	case DocContext:
		have = r.context.log.vv
	case DocSettings:
		have = r.settings.log.vv
	default:
		return
	}
	clamped := VersionVector{}
	for rep, n := range vv {
		if rep != p.id {
			n = min(n, have[rep])
		}
		if n > 0 {
			clamped[rep] = n
		}
	}
	if prev, ok := r.reports[doc][p.id]; ok && !replace {
		clamped.Merge(prev)
	}
	r.reports[doc][p.id] = clamped
}

// Handle decodes and applies one upstream message from the holder of
// token. Failures are *Error values carrying an HTTP status.
func (r *Room) Handle(ctx context.Context, token string, raw []byte) (Reply, error) {
	m, payload, err := DecodeClientMessage(raw)
	if err != nil {
		return Reply{}, errf(http.StatusBadRequest, "bad_request", "%v", err)
	}
	return r.handle(ctx, token, m, payload)
}

func (r *Room) handle(ctx context.Context, token string, m ClientMessage, payload any) (Reply, error) {
	var (
		reply  Reply
		apiErr *Error
	)
	err := r.exec(ctx, func() {
		p := r.authenticate(token)
		if p == nil {
			apiErr = errUnauthorized
			return
		}
		now := r.now()
		if ok, wait := p.bucket.allow(now); !ok {
			r.stats.RateLimited++
			apiErr = &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "slow down", RetryAfter: wait}
			return
		}
		if m.Seq != 0 {
			if m.Seq <= p.lastSeq {
				reply = Reply{OK: true, Duplicate: true}
				return
			}
		}
		reply, apiErr = r.dispatch(p, m, payload, now)
		if apiErr != nil {
			r.stats.Rejected++
			return
		}
		if m.Seq != 0 {
			p.lastSeq = m.Seq
		}
	})
	if err != nil {
		return Reply{}, errClosed
	}
	if apiErr != nil {
		return Reply{}, apiErr
	}
	return reply, nil
}

func forbidden(p *participant, what string) *Error {
	return errf(http.StatusForbidden, "forbidden", "a %s may not %s", p.role, what)
}

func (r *Room) dispatch(p *participant, m ClientMessage, payload any, now time.Time) (Reply, *Error) {
	ok := Reply{OK: true}
	switch m.Type {
	case MsgHeartbeat:
		if hb, _ := payload.(*Heartbeat); hb != nil {
			for doc, vv := range hb.VV {
				r.report(p, doc, vv, false)
			}
		}
		r.setOnline(p, now)
		return ok, nil
	case MsgLeave:
		for s := range r.subs {
			if s.p == p {
				r.dropSub(s, DisconnectEvent{Reason: ReasonLeft})
			}
		}
		r.setOffline(p, ReasonLeft, now)
		return ok, nil
	}
	r.setOnline(p, now)
	switch m.Type {
	case MsgPresence:
		u := payload.(*PresenceUpdate)
		if u.Cursor != nil {
			p.cursor = u.Cursor
		}
		if u.Selection != nil {
			p.sel = u.Selection
		}
		if u.Typing != nil {
			if *u.Typing {
				p.typingUntil = now.Add(r.opts.TypingTimeout)
			} else {
				p.typingUntil = time.Time{}
			}
		}
		if u.Status != "" {
			p.status = u.Status
		}
		r.emit(EvPresence, p.id, p.info(), true)
		return ok, nil
	case MsgChat:
		if r.opts.ViewersReadOnly && !p.role.AtLeast(RoleEditor) {
			return Reply{}, forbidden(p, "chat in this room")
		}
		c := payload.(*ChatPost)
		msg := ChatMessage{ID: r.seq + 1, From: p.id, Name: p.name, Text: c.Text, ReplyTo: c.ReplyTo, Attachments: c.Attachments, Time: now}
		ok.Seq = r.emit(EvChat, p.id, msg, false)
		r.chat = append(r.chat, msg)
		if over := len(r.chat) - r.opts.ChatHistory; over > 0 {
			clear(r.chat[:over])
			r.chat = slices.Delete(r.chat, 0, over)
		}
		return ok, nil
	case MsgOps:
		return r.handleOps(p, payload.(*OpsMessage))
	case MsgSync:
		return r.handleSync(p, payload.(*SyncRequest))
	case MsgVote:
		return r.handleVote(p, payload.(*VoteRequest), now)
	case MsgSubmit:
		if !p.role.AtLeast(RoleEditor) {
			return Reply{}, forbidden(p, "submit the prompt")
		}
		text := r.prompt.String()
		if s := payload.(*SubmitRequest); s.Expect != nil && *s.Expect != text {
			return Reply{}, errf(http.StatusConflict, "prompt_changed", "the prompt changed since you last saw it")
		}
		if text == "" {
			return Reply{}, errf(http.StatusBadRequest, "empty_prompt", "the prompt is empty")
		}
		ok.Seq = r.emit(EvSubmitted, p.id, Submission{By: p.id, Text: text}, false)
		if r.opts.ClearOnSubmit {
			if op, err := r.prompt.Delete(0, r.prompt.Len()); err == nil {
				r.emit(EvOps, ServerReplica, OpsEvent{Doc: DocPrompt, Ops: mustJSON([]TextOp{op})}, false)
			}
		}
		if cb := r.opts.OnSubmit; cb != nil {
			by := p.info()
			go cb(r, by, text)
		}
		return ok, nil
	case MsgSetRole:
		req := payload.(*SetRoleRequest)
		if p.role != RoleOwner {
			return Reply{}, forbidden(p, "change roles")
		}
		t, found := r.participants[req.Participant]
		if !found {
			return Reply{}, errf(http.StatusNotFound, "no_participant", "no participant %q", req.Participant)
		}
		if t.role == RoleOwner && req.Role != RoleOwner && r.owners() == 1 {
			return Reply{}, errf(http.StatusConflict, "last_owner", "the room must keep an owner")
		}
		if t.role != req.Role {
			wasEditor := t.role.AtLeast(RoleEditor)
			t.role = req.Role
			if !t.role.AtLeast(RoleEditor) {
				r.forgetReplica(t)
			} else if !wasEditor {
				// The new editor's view may lag; an empty report holds back
				// garbage collection until it reports what it has seen.
				for _, doc := range []string{DocPrompt, DocContext, DocSettings} {
					r.reports[doc][t.id] = VersionVector{}
					delete(t.stale, doc)
				}
			}
			ok.Seq = r.emit(EvRoleChanged, p.id, RoleChange{Participant: t.id, Role: t.role, By: p.id}, false)
		}
		return ok, nil
	case MsgKick:
		req := payload.(*KickRequest)
		if p.role != RoleOwner {
			return Reply{}, forbidden(p, "remove participants")
		}
		t, found := r.participants[req.Participant]
		if !found {
			return Reply{}, errf(http.StatusNotFound, "no_participant", "no participant %q", req.Participant)
		}
		if t.role == RoleOwner {
			return Reply{}, errf(http.StatusConflict, "is_owner", "demote an owner before removing them")
		}
		for s := range r.subs {
			if s.p == t {
				r.dropSub(s, DisconnectEvent{Reason: ReasonKicked, Detail: req.Reason})
			}
		}
		r.forgetReplica(t)
		delete(r.participants, t.id)
		reason := ReasonKicked
		if req.Reason != "" {
			reason += ": " + req.Reason
		}
		ok.Seq = r.emit(EvLeft, p.id, LeftEvent{ID: t.id, Reason: reason}, false)
		return ok, nil
	}
	return Reply{}, errf(http.StatusBadRequest, "bad_request", "unknown message type %q", m.Type)
}

func (r *Room) owners() int {
	n := 0
	for _, p := range r.participants {
		if p.role == RoleOwner {
			n++
		}
	}
	return n
}

// forgetReplica removes p's replica from the stability frontier and
// discards its buffered operations; it must resync before editing again.
func (r *Room) forgetReplica(p *participant, docs ...string) {
	if len(docs) == 0 {
		docs = []string{DocPrompt, DocContext, DocSettings}
	}
	for _, doc := range docs {
		delete(r.reports[doc], p.id)
		p.stale[doc] = true
		switch doc {
		case DocPrompt:
			r.prompt.DropPending(p.id)
		case DocContext:
			r.context.DropPending(p.id)
		case DocSettings:
			r.settings.DropPending(p.id)
		}
	}
}

func (r *Room) handleOps(p *participant, m *OpsMessage) (Reply, *Error) {
	need := RoleEditor
	if m.Doc == DocSettings {
		need = RoleOwner
	}
	if !p.role.AtLeast(need) {
		return Reply{}, forbidden(p, "edit the "+m.Doc)
	}
	if p.stale[m.Doc] {
		return Reply{}, errf(http.StatusConflict, "resync_required", "your %s replica is out of date; sync with reset and rebase your edits", m.Doc)
	}
	decoded, err := decodeOps(m.Doc, m.Ops)
	if err != nil {
		return Reply{}, errf(http.StatusBadRequest, "bad_request", "%v", err)
	}
	var (
		applied json.RawMessage
		n       int
		pending int
		ierr    error
	)
	switch ops := decoded.(type) {
	case []TextOp:
		for _, op := range ops {
			if op.ID.Replica != p.id {
				return Reply{}, errf(http.StatusForbidden, "foreign_op", "operation %s is not yours", op.ID)
			}
		}
		var out []TextOp
		out, ierr = r.prompt.Apply(ops...)
		n, applied, pending = len(out), mustJSON(out), r.prompt.log.pendingFrom(p.id)
	case []SetOp:
		for _, op := range ops {
			if op.ID.Replica != p.id {
				return Reply{}, errf(http.StatusForbidden, "foreign_op", "operation %s is not yours", op.ID)
			}
		}
		var out []SetOp
		out, ierr = r.context.Apply(ops...)
		n, applied, pending = len(out), mustJSON(out), r.context.log.pendingFrom(p.id)
	case []MapOp:
		limit := r.now().Add(r.opts.MaxDrift).UnixNano()
		for _, op := range ops {
			if op.ID.Replica != p.id || op.TS.Node != p.id {
				return Reply{}, errf(http.StatusForbidden, "foreign_op", "operation %s is not yours", op.ID)
			}
			if op.TS.Wall > limit {
				return Reply{}, errf(http.StatusBadRequest, "clock_drift", "operation %s is stamped too far in the future", op.ID)
			}
		}
		var out []MapOp
		out, ierr = r.settings.Apply(ops...)
		n, applied, pending = len(out), mustJSON(out), r.settings.log.pendingFrom(p.id)
	}
	if n > 0 {
		r.emit(EvOps, p.id, OpsEvent{Doc: m.Doc, Ops: applied}, false)
	}
	if ierr != nil {
		r.forgetReplica(p, m.Doc)
		status, code := http.StatusConflict, "resync_required"
		if errors.Is(ierr, ErrInvalidOp) {
			status = http.StatusUnprocessableEntity
		}
		return Reply{}, errf(status, code, "%v; sync with reset and rebase your edits", ierr)
	}
	r.report(p, m.Doc, m.VV, false)
	return Reply{OK: true, Seq: r.seq, Applied: n, Pending: pending}, nil
}

func (r *Room) handleSync(p *participant, req *SyncRequest) (Reply, *Error) {
	out := SyncReply{Doc: req.Doc}
	snapshot := req.Reset
	if !snapshot {
		var (
			ops any
			err error
		)
		switch req.Doc {
		case DocPrompt:
			ops, err = r.prompt.Delta(req.VV)
		case DocContext:
			ops, err = r.context.Delta(req.VV)
		case DocSettings:
			ops, err = r.settings.Delta(req.VV)
		}
		if errors.Is(err, ErrSnapshotRequired) {
			snapshot = true
		} else {
			out.Ops = mustJSON(ops)
		}
	}
	switch req.Doc {
	case DocPrompt:
		out.VV = r.prompt.VersionVector()
		if snapshot {
			out.Snapshot = mustJSON(r.prompt.Snapshot(false))
		}
	case DocContext:
		out.VV = r.context.VersionVector()
		if snapshot {
			out.Snapshot = mustJSON(r.context.Snapshot(false))
		}
	case DocSettings:
		out.VV = r.settings.VersionVector()
		if snapshot {
			out.Snapshot = mustJSON(r.settings.Snapshot(false))
		}
	}
	if snapshot {
		out.Ops = nil
		if p.role.AtLeast(RoleEditor) {
			switch req.Doc {
			case DocPrompt:
				r.prompt.DropPending(p.id)
			case DocContext:
				r.context.DropPending(p.id)
			case DocSettings:
				r.settings.DropPending(p.id)
			}
			delete(p.stale, req.Doc)
			r.report(p, req.Doc, out.VV, true)
		}
	} else {
		r.report(p, req.Doc, req.VV, false)
	}
	return Reply{OK: true, Seq: r.seq, Result: mustJSON(out)}, nil
}

func (r *Room) eligibleVoters() int {
	n := 0
	for _, p := range r.participants {
		if p.role.AtLeast(RoleEditor) {
			n++
		}
	}
	return n
}

func (r *Room) handleVote(p *participant, req *VoteRequest, now time.Time) (Reply, *Error) {
	if !p.role.AtLeast(RoleEditor) {
		return Reply{}, forbidden(p, "vote on tool calls")
	}
	a, ok := r.approvals[req.ApprovalID]
	if !ok {
		return Reply{}, errf(http.StatusNotFound, "no_approval", "no pending approval %q", req.ApprovalID)
	}
	a.vote(Vote{Participant: p.id, Name: p.name, Role: p.role, Approve: req.Approve, Comment: req.Comment, Time: now})
	seq := r.emit(EvApprovalVote, p.id, a.view, false)
	if d := r.policy.Evaluate(a.view.Votes, r.eligibleVoters()); d != DecisionPending {
		reason := "quorum reached"
		if d == DecisionDenied && !satisfied(r.policy.Deny, a.view.Votes, false) {
			reason = "every eligible participant voted without reaching quorum"
		}
		r.finishApproval(a, d, reason)
	}
	return Reply{OK: true, Seq: seq}, nil
}

func (r *Room) finishApproval(a *approval, d Decision, reason string) {
	delete(r.approvals, a.view.ID)
	o := a.resolve(d, reason)
	r.emit(EvApprovalResolved, "", o, false)
}

func (r *Room) tick(now time.Time) {
	present := false
	for _, p := range r.participants {
		if !p.typingUntil.IsZero() && now.After(p.typingUntil) {
			p.typingUntil = time.Time{}
			r.emit(EvPresence, p.id, p.info(), true)
		}
		if p.online {
			quiet := now.Sub(p.lastSeen) > r.opts.HeartbeatTimeout
			switch {
			case p.streams == 0 && quiet:
				r.setOffline(p, ReasonTimeout, now)
			case quiet && p.status == "active":
				p.status = "idle"
				r.emit(EvPresence, p.id, p.info(), true)
			}
		}
		if p.online {
			present = true
		}
		if !p.online && !p.offlineAt.IsZero() && now.Sub(p.offlineAt) > r.opts.ReplicaEvictAfter && p.role.AtLeast(RoleEditor) {
			if _, tracked := r.reports[DocPrompt][p.id]; tracked {
				r.forgetReplica(p)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(r.approvals)) {
		a := r.approvals[id]
		if !a.view.Deadline.IsZero() && now.After(a.view.Deadline) {
			r.finishApproval(a, DecisionExpired, "no quorum before the deadline")
		}
	}
	if now.Sub(r.lastGC) >= r.opts.GCInterval {
		r.collectGarbage()
		r.lastGC = now
	}
	if present || len(r.subs) > 0 {
		r.idleSince = now
	} else if r.opts.RoomIdleTTL > 0 && now.Sub(r.idleSince) > r.opts.RoomIdleTTL {
		go r.Close("idle")
	}
}

// collectGarbage runs tombstone collection and log compaction against the
// reports of every replica that may still edit.
func (r *Room) collectGarbage() GCStats {
	reports := func(doc string) map[string]VersionVector {
		out := map[string]VersionVector{}
		for _, p := range r.participants {
			if !p.role.AtLeast(RoleEditor) || p.stale[doc] {
				continue
			}
			// A replica with no report has never received state; it will
			// start from a snapshot, so it cannot hold concurrent edits.
			if rep, ok := r.reports[doc][p.id]; ok {
				out[p.id] = rep
			}
		}
		return out
	}
	st := r.prompt.CollectGarbage(reports(DocPrompt))
	r.stats.LastGC = st
	for doc, compact := range map[string]func(VersionVector) int{DocContext: r.context.Compact, DocSettings: r.settings.Compact} {
		reps := reports(doc)
		vs := []VersionVector{}
		safe := true
		var own VersionVector
		if doc == DocContext {
			own = r.context.log.vv
		} else {
			own = r.settings.log.vv
		}
		vs = append(vs, own)
		for id, rep := range reps {
			if own[id] < rep[id] {
				safe = false
			}
			vs = append(vs, rep)
		}
		if safe {
			compact(Meet(vs...))
		}
	}
	return st
}

// Invite adds a participant and returns their credentials.
func (r *Room) Invite(ctx context.Context, name string, role Role) (Credentials, error) {
	if !role.Valid() {
		return Credentials{}, fmt.Errorf("collab: invalid role %d", uint8(role))
	}
	name, err := cleanName(name, role.String(), MaxNameRunes)
	if err != nil {
		return Credentials{}, err
	}
	var (
		creds Credentials
		aerr  error
	)
	if err := r.exec(ctx, func() { creds, aerr = r.addParticipant(name, role) }); err != nil {
		return Credentials{}, err
	}
	return creds, aerr
}

// inviteAs adds a participant on behalf of the holder of token, who must
// be an owner.
func (r *Room) inviteAs(ctx context.Context, token, name string, role Role) (Credentials, *Error) {
	var (
		creds  Credentials
		apiErr *Error
	)
	if !role.Valid() {
		return Credentials{}, errf(http.StatusBadRequest, "bad_request", "invalid role")
	}
	name, err := cleanName(name, role.String(), MaxNameRunes)
	if err != nil {
		return Credentials{}, errf(http.StatusBadRequest, "bad_request", "%v", err)
	}
	err = r.exec(ctx, func() {
		p := r.authenticate(token)
		switch {
		case p == nil:
			apiErr = errUnauthorized
		case p.role != RoleOwner:
			apiErr = forbidden(p, "invite participants")
		default:
			var e error
			if creds, e = r.addParticipant(name, role); e != nil {
				var ae *Error
				if errors.As(e, &ae) {
					apiErr = ae
				} else {
					apiErr = errf(http.StatusInternalServerError, "internal", "%v", e)
				}
			}
		}
	})
	if err != nil {
		return Credentials{}, errClosed
	}
	return creds, apiErr
}

// whoami returns the participant holding token.
func (r *Room) whoami(ctx context.Context, token string) (ParticipantInfo, RoomInfo, []ParticipantInfo, *Error) {
	var (
		me     ParticipantInfo
		info   RoomInfo
		list   []ParticipantInfo
		apiErr *Error
	)
	err := r.exec(ctx, func() {
		p := r.authenticate(token)
		if p == nil {
			apiErr = errUnauthorized
			return
		}
		me, info, list = p.info(), r.info(), r.participantList()
	})
	if err != nil {
		return me, info, nil, errClosed
	}
	return me, info, list, apiErr
}

// Info describes the room.
func (r *Room) Info(ctx context.Context) (RoomInfo, error) {
	var info RoomInfo
	err := r.exec(ctx, func() { info = r.info() })
	return info, err
}

// Participants lists the room's participants.
func (r *Room) Participants(ctx context.Context) ([]ParticipantInfo, error) {
	var out []ParticipantInfo
	err := r.exec(ctx, func() { out = r.participantList() })
	return out, err
}

// Stats returns operational counters.
func (r *Room) Stats(ctx context.Context) (RoomStats, error) {
	var st RoomStats
	err := r.exec(ctx, func() {
		st = r.stats
		info := r.info()
		st.Seq, st.Participants, st.Online = r.seq, info.Participants, info.Online
		st.Streams, st.PendingApprovals, st.Prompt = len(r.subs), len(r.approvals), r.prompt.Stats()
	})
	return st, err
}

// Prompt returns the shared prompt text.
func (r *Room) Prompt(ctx context.Context) (string, error) {
	var s string
	err := r.exec(ctx, func() { s = r.prompt.String() })
	return s, err
}

// SetPrompt replaces the shared prompt as the server replica.
func (r *Room) SetPrompt(ctx context.Context, text string) error {
	var ierr error
	err := r.exec(ctx, func() {
		var ops []TextOp
		if n := r.prompt.Len(); n > 0 {
			op, e := r.prompt.Delete(0, n)
			if e != nil {
				ierr = e
				return
			}
			ops = append(ops, op)
		}
		if text != "" {
			op, e := r.prompt.Insert(0, text)
			if e != nil {
				ierr = e
				return
			}
			ops = append(ops, op)
		}
		if len(ops) > 0 {
			r.emit(EvOps, ServerReplica, OpsEvent{Doc: DocPrompt, Ops: mustJSON(ops)}, false)
		}
	})
	return errors.Join(err, ierr)
}

// SetSetting writes a shared setting as the server replica.
func (r *Room) SetSetting(ctx context.Context, key string, value any) error {
	var ierr error
	err := r.exec(ctx, func() {
		op, e := r.settings.Set(key, value)
		if e != nil {
			ierr = e
			return
		}
		r.emit(EvOps, ServerReplica, OpsEvent{Doc: DocSettings, Ops: mustJSON([]MapOp{op})}, false)
	})
	return errors.Join(err, ierr)
}

// Settings returns the shared settings.
func (r *Room) Settings(ctx context.Context) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	err := r.exec(ctx, func() { out = r.settings.Entries() })
	return out, err
}

// ContextItems returns the shared context set.
func (r *Room) ContextItems(ctx context.Context) ([]string, error) {
	var out []string
	err := r.exec(ctx, func() { out = r.context.Elements() })
	return out, err
}

// Publish broadcasts an agent event (streamed text, tool progress, ...)
// to every participant.
func (r *Room) Publish(ctx context.Context, kind string, data any) error {
	if kind == "" {
		return errors.New("collab: agent event needs a kind")
	}
	raw := mustJSON(data)
	return r.exec(ctx, func() { r.emit(EvAgent, "agent", AgentEvent{Kind: kind, Data: raw}, false) })
}

// RequestApproval asks the room to approve a tool call and blocks until
// the quorum policy decides, the deadline passes, ctx ends (the request is
// then cancelled) or the room closes.
func (r *Room) RequestApproval(ctx context.Context, req ApprovalRequest) (Outcome, error) {
	if req.Tool == "" {
		return Outcome{}, errors.New("collab: approval request needs a tool")
	}
	a := &approval{done: make(chan Outcome, 1)}
	var ierr error
	err := r.exec(ctx, func() {
		if req.ID == "" {
			req.ID = "a_" + randomHex(8)
		}
		if _, dup := r.approvals[req.ID]; dup {
			ierr = fmt.Errorf("collab: approval %q is already pending", req.ID)
			return
		}
		now := r.now()
		a.view = ApprovalView{ApprovalRequest: req, Requested: now, Votes: []Vote{}, Decision: DecisionPending}
		if r.policy.Timeout > 0 {
			a.view.Deadline = now.Add(r.policy.Timeout)
		}
		r.approvals[req.ID] = a
		r.emit(EvApprovalRequest, "agent", a.view, false)
	})
	if err != nil {
		return Outcome{ID: req.ID, Decision: DecisionCancelled, Reason: "room closed"}, err
	}
	if ierr != nil {
		return Outcome{}, ierr
	}
	select {
	case o := <-a.done:
		return o, nil
	case <-ctx.Done():
		_ = r.exec(context.Background(), func() {
			if cur, ok := r.approvals[a.view.ID]; ok && cur == a {
				r.finishApproval(a, DecisionCancelled, "requester cancelled")
			}
		})
		select {
		case o := <-a.done:
			if o.Decision != DecisionCancelled {
				return o, nil
			}
			return o, ctx.Err()
		default:
			return Outcome{ID: req.ID, Decision: DecisionCancelled, Reason: "requester cancelled"}, ctx.Err()
		}
	case <-r.done:
		select {
		case o := <-a.done:
			return o, nil
		default:
			return Outcome{ID: req.ID, Decision: DecisionCancelled, Reason: "room closed"}, ErrRoomClosed
		}
	}
}
