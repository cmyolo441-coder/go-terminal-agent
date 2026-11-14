package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testEnv struct {
	t     *testing.T
	hub   *Hub
	srv   *httptest.Server
	room  *Room
	owner *Client
}

func newEnv(t *testing.T, mutate func(*Options)) *testEnv {
	t.Helper()
	o := Options{TickInterval: 20 * time.Millisecond, RateLimit: -1}
	if mutate != nil {
		mutate(&o)
	}
	hub, err := NewHub(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hub.Handler())
	t.Cleanup(func() {
		_ = hub.Close()
		srv.Close()
	})
	resp, err := CreateRoom(context.Background(), srv.Client(), srv.URL, hub.AdminToken(), RoomConfig{Title: "pair", OwnerName: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	room, ok := hub.Room(resp.Room.ID)
	if !ok {
		t.Fatal("room missing")
	}
	return &testEnv{t: t, hub: hub, srv: srv, room: room, owner: &Client{BaseURL: srv.URL, RoomID: room.ID(), Token: resp.Owner.Token, HTTP: srv.Client()}}
}

func (e *testEnv) invite(name string, role Role) *Client {
	e.t.Helper()
	creds, err := e.owner.Invite(context.Background(), name, role)
	if err != nil {
		e.t.Fatal(err)
	}
	return &Client{BaseURL: e.srv.URL, RoomID: e.room.ID(), Token: creds.Token, HTTP: e.srv.Client()}
}

func apiCode(err error) string {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	if err == nil {
		return ""
	}
	return "other: " + err.Error()
}

// collector reads a stream into a channel.
type collector struct {
	events chan Event
	stream *Stream
	done   chan error
}

func collect(t *testing.T, c *Client, lastID uint64, resume bool) *collector {
	t.Helper()
	s, err := c.Open(context.Background(), lastID, resume)
	if err != nil {
		t.Fatal(err)
	}
	col := &collector{events: make(chan Event, 4096), stream: s, done: make(chan error, 1)}
	go func() {
		for {
			ev, err := s.Next()
			if err != nil {
				col.done <- err
				close(col.events)
				return
			}
			col.events <- ev
		}
	}()
	t.Cleanup(func() { s.Close() })
	return col
}

// waitFor returns the first event matching pred.
func (c *collector) waitFor(t *testing.T, pred func(Event) bool) Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				t.Fatal("stream ended while waiting")
			}
			if pred(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for event")
		}
	}
}

func ofType(typ string) func(Event) bool { return func(e Event) bool { return e.Type == typ } }

func TestQuorumPolicyEvaluate(t *testing.T) {
	p := DefaultQuorumPolicy()
	v := func(role Role, ok bool) Vote { return Vote{Role: role, Approve: ok} }
	cases := []struct {
		votes    []Vote
		eligible int
		want     Decision
	}{
		{nil, 3, DecisionPending},
		{[]Vote{v(RoleOwner, true)}, 3, DecisionApproved},
		{[]Vote{v(RoleEditor, true)}, 3, DecisionPending},
		{[]Vote{v(RoleEditor, true), v(RoleEditor, true)}, 3, DecisionApproved},
		{[]Vote{v(RoleEditor, true), v(RoleOwner, false)}, 3, DecisionDenied},
		{[]Vote{v(RoleEditor, false), v(RoleEditor, false)}, 3, DecisionDenied},
		{[]Vote{v(RoleEditor, true), v(RoleEditor, false)}, 2, DecisionDenied}, // everyone voted
		{[]Vote{v(RoleViewer, true), v(RoleViewer, true)}, 3, DecisionPending},
	}
	for i, c := range cases {
		if got := p.Evaluate(c.votes, c.eligible); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
	if (QuorumPolicy{}).Validate() == nil {
		t.Error("empty policy validated")
	}
	if (QuorumPolicy{Approve: []QuorumRule{{MinRole: RoleViewer, Count: 1}}}).Validate() == nil {
		t.Error("viewer quorum validated")
	}
}

func TestApprovalQuorumOverHTTP(t *testing.T) {
	env := newEnv(t, nil)
	ed1, ed2 := env.invite("bob", RoleEditor), env.invite("cy", RoleEditor)
	viewer := env.invite("dee", RoleViewer)
	ctx := context.Background()

	request := func(id string) chan Outcome {
		ch := make(chan Outcome, 1)
		go func() {
			o, err := env.room.RequestApproval(ctx, ApprovalRequest{ID: id, Tool: "bash", Summary: "rm -rf build"})
			if err != nil {
				t.Error(err)
			}
			ch <- o
		}()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			n := 0
			_ = env.room.exec(ctx, func() { n = len(env.room.approvals) })
			if n > 0 {
				return ch
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("approval never registered")
		return ch
	}
	vote := func(c *Client, id string, ok bool) error {
		_, err := c.Send(ctx, MsgVote, VoteRequest{ApprovalID: id, Approve: ok})
		return err
	}

	// Two editors approve.
	ch := request("a1")
	if err := vote(viewer, "a1", true); apiCode(err) != "forbidden" {
		t.Fatalf("viewer vote: %v", err)
	}
	if err := vote(ed1, "a1", true); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-ch:
		t.Fatalf("resolved after one editor: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}
	if err := vote(ed2, "a1", true); err != nil {
		t.Fatal(err)
	}
	if o := <-ch; !o.Approved() || len(o.Votes) != 2 {
		t.Fatalf("outcome %+v", o)
	}
	if err := vote(ed1, "a1", true); apiCode(err) != "no_approval" {
		t.Fatalf("vote after resolution: %v", err)
	}

	// The owner alone denies; an editor changing their mind counts once.
	ch = request("a2")
	_ = vote(ed1, "a2", true)
	_ = vote(ed1, "a2", false)
	if err := vote(env.owner, "a2", false); err != nil {
		t.Fatal(err)
	}
	if o := <-ch; o.Decision != DecisionDenied || len(o.Votes) != 2 {
		t.Fatalf("outcome %+v", o)
	}

	// Cancellation by the requester.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan Outcome, 1)
	go func() {
		o, _ := env.room.RequestApproval(cctx, ApprovalRequest{ID: "a3", Tool: "bash"})
		done <- o
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if o := <-done; o.Decision != DecisionCancelled {
		t.Fatalf("outcome %+v", o)
	}
}

func TestApprovalExpires(t *testing.T) {
	env := newEnv(t, func(o *Options) {
		o.Quorum = DefaultQuorumPolicy()
		o.Quorum.Timeout = 100 * time.Millisecond
	})
	o, err := env.room.RequestApproval(context.Background(), ApprovalRequest{Tool: "write"})
	if err != nil || o.Decision != DecisionExpired {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestRoleEnforcement(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.ViewersReadOnly = true })
	ctx := context.Background()
	editor, viewer := env.invite("ed", RoleEditor), env.invite("vi", RoleViewer)
	info, _ := editor.Info(ctx)
	edID := info.You.ID
	vinfo, _ := viewer.Info(ctx)

	vt := NewText(vinfo.You.ID)
	op, _ := vt.Insert(0, "hi")
	opsMsg := func(doc string, ops any) OpsMessage { return OpsMessage{Doc: doc, Ops: mustJSON(ops)} }
	if _, err := viewer.Send(ctx, MsgOps, opsMsg(DocPrompt, []TextOp{op})); apiCode(err) != "forbidden" {
		t.Fatalf("viewer edit: %v", err)
	}
	if _, err := viewer.Send(ctx, MsgChat, ChatPost{Text: "hey"}); apiCode(err) != "forbidden" {
		t.Fatalf("read-only viewer chat: %v", err)
	}
	if _, err := viewer.Send(ctx, MsgSync, SyncRequest{Doc: DocPrompt}); err != nil {
		t.Fatalf("viewer sync: %v", err)
	}
	// An editor may not forge another replica's operations.
	if _, err := editor.Send(ctx, MsgOps, opsMsg(DocPrompt, []TextOp{op})); apiCode(err) != "foreign_op" {
		t.Fatalf("foreign op: %v", err)
	}
	et := NewText(edID)
	eop, _ := et.Insert(0, "hello")
	if r, err := editor.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON([]TextOp{eop}), VV: et.VersionVector()}); err != nil || r.Applied != 1 {
		t.Fatalf("editor edit: %+v %v", r, err)
	}
	// Settings are owner-only.
	em := NewLWWMap(edID, nil)
	mop, _ := em.Set("model", "x")
	if _, err := editor.Send(ctx, MsgOps, opsMsg(DocSettings, []MapOp{mop})); apiCode(err) != "forbidden" {
		t.Fatalf("editor settings: %v", err)
	}
	if _, err := editor.Send(ctx, MsgSetRole, SetRoleRequest{Participant: vinfo.You.ID, Role: RoleOwner}); apiCode(err) != "forbidden" {
		t.Fatalf("editor set_role: %v", err)
	}
	if _, err := editor.Send(ctx, MsgKick, KickRequest{Participant: vinfo.You.ID}); apiCode(err) != "forbidden" {
		t.Fatalf("editor kick: %v", err)
	}
	if _, err := editor.Invite(ctx, "x", RoleOwner); apiCode(err) != "forbidden" {
		t.Fatalf("editor invite: %v", err)
	}
	// The last owner cannot be demoted.
	oinfo, _ := env.owner.Info(ctx)
	if _, err := env.owner.Send(ctx, MsgSetRole, SetRoleRequest{Participant: oinfo.You.ID, Role: RoleEditor}); apiCode(err) != "last_owner" {
		t.Fatalf("demote last owner: %v", err)
	}
	// Promote the viewer: now it may edit.
	if _, err := env.owner.Send(ctx, MsgSetRole, SetRoleRequest{Participant: vinfo.You.ID, Role: RoleEditor}); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON([]TextOp{op})}); err != nil {
		t.Fatalf("promoted edit: %v", err)
	}
	// Kick: the token stops working and the stream ends with a reason.
	col := collect(t, editor, 0, false)
	if _, err := env.owner.Send(ctx, MsgKick, KickRequest{Participant: edID, Reason: "spam"}); err != nil {
		t.Fatal(err)
	}
	ev := col.waitFor(t, ofType(EvDisconnect))
	var d DisconnectEvent
	_ = json.Unmarshal(ev.Data, &d)
	if d.Reason != ReasonKicked {
		t.Fatalf("reason %+v", d)
	}
	if _, err := editor.Send(ctx, MsgHeartbeat, nil); apiCode(err) != "unauthorized" {
		t.Fatalf("kicked token: %v", err)
	}
	// Bad tokens and admin endpoints.
	bad := &Client{BaseURL: env.srv.URL, RoomID: env.room.ID(), Token: "gtc_nope", HTTP: env.srv.Client()}
	if _, err := bad.Info(ctx); apiCode(err) != "unauthorized" {
		t.Fatal(err)
	}
	if _, err := CreateRoom(ctx, env.srv.Client(), env.srv.URL, "wrong", RoomConfig{}); apiCode(err) != "unauthorized" {
		t.Fatal(err)
	}
	p, _ := env.room.Prompt(ctx)
	if p != "hihello" && p != "hellohi" {
		t.Fatalf("prompt %q", p)
	}
}

func TestAuthFailuresThrottled(t *testing.T) {
	env := newEnv(t, nil)
	bad := &Client{BaseURL: env.srv.URL, RoomID: env.room.ID(), Token: "gtc_bad", HTTP: env.srv.Client()}
	var last error
	for range DefaultAuthFailBurst + 5 {
		_, last = bad.Info(context.Background())
	}
	var ae *Error
	if !errors.As(last, &ae) || ae.Status != http.StatusTooManyRequests || ae.RetryAfter <= 0 {
		t.Fatalf("not throttled: %v", last)
	}
}

func TestRateLimitPerParticipant(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.RateLimit, o.RateBurst = 1, 3 })
	ctx := context.Background()
	other := env.invite("bob", RoleEditor)
	var limited error
	for range 6 {
		if _, err := env.owner.Send(ctx, MsgHeartbeat, nil); err != nil {
			limited = err
			break
		}
	}
	var ae *Error
	if !errors.As(limited, &ae) || ae.Code != "rate_limited" || ae.RetryAfter <= 0 {
		t.Fatalf("expected rate limit, got %v", limited)
	}
	// Buckets are per participant.
	if _, err := other.Send(ctx, MsgHeartbeat, nil); err != nil {
		t.Fatalf("other participant throttled: %v", err)
	}
}

func TestDuplicateMessageIgnored(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	body := []byte(`{"type":"chat","seq":7,"data":{"text":"once"}}`)
	r1, err := env.room.Handle(ctx, env.owner.Token, body)
	if err != nil || r1.Duplicate {
		t.Fatal(r1, err)
	}
	r2, err := env.room.Handle(ctx, env.owner.Token, body)
	if err != nil || !r2.Duplicate {
		t.Fatal(r2, err)
	}
	st, _ := env.room.Stats(ctx)
	if st.Seq != r1.Seq {
		t.Fatal("duplicate produced an event")
	}
}

func TestSSEResumeAfterDisconnect(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.ReplayBuffer = 16 })
	ctx := context.Background()
	col := collect(t, env.owner, 0, false)
	w := col.waitFor(t, ofType(EvWelcome))
	var welcome Welcome
	_ = json.Unmarshal(w.Data, &welcome)
	if welcome.Resumed || welcome.Prompt == nil {
		t.Fatalf("fresh welcome: %+v", welcome)
	}
	for i := range 3 {
		if _, err := env.owner.Send(ctx, MsgChat, ChatPost{Text: fmt.Sprint("m", i)}); err != nil {
			t.Fatal(err)
		}
	}
	var lastSeen uint64
	for range 3 {
		lastSeen = col.waitFor(t, ofType(EvChat)).Seq
	}
	col.stream.Close()
	// Events while disconnected.
	var missed []uint64
	for i := range 5 {
		r, err := env.owner.Send(ctx, MsgChat, ChatPost{Text: fmt.Sprint("late", i)})
		if err != nil {
			t.Fatal(err)
		}
		missed = append(missed, r.Seq)
	}
	col2 := collect(t, env.owner, lastSeen, true)
	welcome = Welcome{}
	_ = json.Unmarshal(col2.waitFor(t, ofType(EvWelcome)).Data, &welcome)
	if !welcome.Resumed || welcome.Prompt != nil {
		t.Fatalf("resume welcome: resumed=%v", welcome.Resumed)
	}
	var got []uint64
	for len(got) < len(missed) {
		ev := col2.waitFor(t, func(e Event) bool { return e.Seq != 0 })
		got = append(got, ev.Seq)
		if ev.Seq <= lastSeen {
			t.Fatalf("replayed already-seen event %d", ev.Seq)
		}
	}
	// Every missed chat is replayed in order (left/joined events may sit
	// between them).
	j := 0
	for _, s := range got {
		if j < len(missed) && s == missed[j] {
			j++
		}
	}
	for j < len(missed) {
		ev := col2.waitFor(t, func(e Event) bool { return e.Seq != 0 })
		if ev.Seq == missed[j] {
			j++
		}
	}
	// Too old to resume: a fresh welcome instead.
	for i := range 40 {
		_, _ = env.owner.Send(ctx, MsgChat, ChatPost{Text: fmt.Sprint("flood", i)})
	}
	col3 := collect(t, env.owner, lastSeen, true)
	welcome = Welcome{}
	_ = json.Unmarshal(col3.waitFor(t, ofType(EvWelcome)).Data, &welcome)
	if welcome.Resumed || welcome.Prompt == nil || len(welcome.Chat) == 0 {
		t.Fatal("stale Last-Event-ID must get a full welcome")
	}
}

func TestClientRunResumes(t *testing.T) {
	env := newEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	seen := map[string]int{}
	welcomes := 0
	errc := make(chan error, 1)
	go func() {
		errc <- env.owner.Run(ctx, func(ev Event) error {
			mu.Lock()
			defer mu.Unlock()
			if ev.Type == EvWelcome {
				welcomes++
			}
			if ev.Type == EvChat {
				var m ChatMessage
				_ = json.Unmarshal(ev.Data, &m)
				seen[m.Text]++
			}
			return nil
		})
	}()
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return welcomes == 1 })
	_, _ = env.owner.Send(ctx, MsgChat, ChatPost{Text: "one"})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return seen["one"] == 1 })
	// Sever every stream server-side as a slow consumer would be.
	_ = env.room.exec(ctx, func() {
		for s := range env.room.subs {
			env.room.dropSub(s, DisconnectEvent{Reason: ReasonSlowConsumer})
		}
	})
	_, _ = env.owner.Send(ctx, MsgChat, ChatPost{Text: "two"})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return seen["two"] == 1 && welcomes == 2 })
	mu.Lock()
	if seen["one"] != 1 {
		t.Fatalf("duplicate delivery after resume: %v", seen)
	}
	mu.Unlock()
	// Closing the room ends Run cleanly.
	env.room.Close("done")
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after room close")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSlowConsumerDisconnected(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.SubscriberBuffer = 4 })
	ctx := context.Background()
	// Register a subscriber nobody reads from.
	sub, _, e := env.room.subscribe(ctx, env.owner.Token, 0, false)
	if e != nil {
		t.Fatal(e)
	}
	for i := range 20 {
		if _, err := env.owner.Send(ctx, MsgChat, ChatPost{Text: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-sub.done:
	case <-time.After(5 * time.Second):
		t.Fatal("slow subscriber was not dropped")
	}
	if sub.reason.Reason != ReasonSlowConsumer {
		t.Fatalf("reason %+v", sub.reason)
	}
	// The dropped client resumes from its last event and gets the rest.
	resumed := collect(t, env.owner, 1, true)
	for range 20 {
		resumed.waitFor(t, ofType(EvChat))
	}
	st, _ := env.room.Stats(ctx)
	if st.SlowDisconnects == 0 {
		t.Fatal("not counted")
	}
	// Ephemeral events are dropped rather than disconnecting.
	sub2, _, _ := env.room.subscribe(ctx, env.owner.Token, 0, false)
	typing := true
	for range 10 {
		_, _ = env.owner.Send(ctx, MsgPresence, PresenceUpdate{Typing: &typing})
	}
	select {
	case <-sub2.done:
		t.Fatal("ephemeral overflow disconnected the stream")
	default:
	}
	st, _ = env.room.Stats(ctx)
	if st.DroppedEphemeral == 0 {
		t.Fatal("ephemeral drops not counted")
	}
}

func TestSlowConsumerOverHTTP(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.SubscriberBuffer = 2; o.WriteTimeout = 200 * time.Millisecond })
	ctx := context.Background()
	// A client that opens the stream but never reads.
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/v1/collab/rooms/"+env.room.ID()+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+env.owner.Token)
	resp, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Loopback socket buffers absorb megabytes; keep sending until the
	// unread stream's queue overflows.
	big := strings.Repeat("x", MaxChatRunes)
	for i := 0; i < 20000; i++ {
		_, _ = env.owner.Send(ctx, MsgChat, ChatPost{Text: big})
		if i%50 == 0 {
			if st, _ := env.room.Stats(ctx); st.SlowDisconnects > 0 {
				break
			}
		}
	}
	waitUntil(t, func() bool {
		st, _ := env.room.Stats(ctx)
		return st.SlowDisconnects > 0 && st.Streams == 0
	})
}

func TestPresenceTimeout(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.HeartbeatTimeout = 150 * time.Millisecond; o.TypingTimeout = 50 * time.Millisecond })
	ctx := context.Background()
	bob := env.invite("bob", RoleEditor)
	watch := collect(t, env.owner, 0, false)
	typing := true
	if _, err := bob.Send(ctx, MsgPresence, PresenceUpdate{Typing: &typing, Cursor: new(3)}); err != nil {
		t.Fatal(err)
	}
	joined := watch.waitFor(t, ofType(EvJoined))
	var who ParticipantInfo
	_ = json.Unmarshal(joined.Data, &who)
	watch.waitFor(t, func(e Event) bool {
		var p ParticipantInfo
		return e.Type == EvPresence && json.Unmarshal(e.Data, &p) == nil && p.ID == who.ID && !p.Typing
	})
	left := watch.waitFor(t, ofType(EvLeft))
	var l LeftEvent
	_ = json.Unmarshal(left.Data, &l)
	if l.ID != who.ID || l.Reason != ReasonTimeout {
		t.Fatalf("left %+v", l)
	}
	// The streaming owner goes idle instead of leaving.
	waitUntil(t, func() bool {
		list, _ := env.room.Participants(ctx)
		for _, p := range list {
			if p.Role == RoleOwner {
				return p.Online && p.Status == "idle"
			}
		}
		return false
	})
}

func TestStaleReplicaMustResync(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	ed := env.invite("ed", RoleEditor)
	info, _ := ed.Info(ctx)
	x := NewText(info.You.ID)
	var ops []TextOp
	for i := range 3 {
		op, _ := x.Insert(i, "a")
		ops = append(ops, op)
	}
	// Only the last op: it buffers, waiting for its predecessors.
	r, err := ed.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON(ops[2:])})
	if err != nil || r.Applied != 0 || r.Pending != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ed.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON(ops[:2])})
	if err != nil || r.Applied != 3 || r.Pending != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	// A broken operation forces a resync.
	bad := TextOp{ID: Dot{info.You.ID, 4}, Kind: TextInsert, Lamport: 1, Text: "dup"}
	if _, err := ed.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON([]TextOp{bad})}); apiCode(err) != "resync_required" {
		t.Fatalf("bad op: %v", err)
	}
	good, _ := x.Insert(0, "z")
	if _, err := ed.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON([]TextOp{good})}); apiCode(err) != "resync_required" {
		t.Fatalf("stale replica accepted: %v", err)
	}
	rep, err := ed.Send(ctx, MsgSync, SyncRequest{Doc: DocPrompt, Reset: true})
	if err != nil {
		t.Fatal(err)
	}
	var sr SyncReply
	_ = json.Unmarshal(rep.Result, &sr)
	var snap TextSnapshot
	if err := json.Unmarshal(sr.Snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	y, err := RestoreText(info.You.ID, snap)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := y.Insert(0, "z")
	if _, err := ed.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON([]TextOp{op}), VV: y.VersionVector()}); err != nil {
		t.Fatalf("after resync: %v", err)
	}
	if p, _ := env.room.Prompt(ctx); p != "zaaa" {
		t.Fatalf("prompt %q", p)
	}
}

func TestSubmitAndAgentEvents(t *testing.T) {
	submitted := make(chan string, 1)
	env := newEnv(t, func(o *Options) {
		o.ClearOnSubmit = true
		o.OnSubmit = func(_ *Room, by ParticipantInfo, text string) { submitted <- by.Name + ":" + text }
	})
	ctx := context.Background()
	col := collect(t, env.owner, 0, false)
	if err := env.room.SetPrompt(ctx, "fix the bug"); err != nil {
		t.Fatal(err)
	}
	wrong := "stale"
	if _, err := env.owner.Send(ctx, MsgSubmit, SubmitRequest{Expect: &wrong}); apiCode(err) != "prompt_changed" {
		t.Fatal(err)
	}
	if _, err := env.owner.Send(ctx, MsgSubmit, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-submitted; got != "ada:fix the bug" {
		t.Fatal(got)
	}
	if p, _ := env.room.Prompt(ctx); p != "" {
		t.Fatalf("not cleared: %q", p)
	}
	if err := env.room.Publish(ctx, "text_delta", map[string]string{"text": "on it"}); err != nil {
		t.Fatal(err)
	}
	if err := env.room.SetSetting(ctx, "model", "opus"); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, ofType(EvSubmitted))
	ev := col.waitFor(t, ofType(EvAgent))
	var ae AgentEvent
	_ = json.Unmarshal(ev.Data, &ae)
	if ae.Kind != "text_delta" {
		t.Fatal(ae)
	}
	s, _ := env.room.Settings(ctx)
	if string(s["model"]) != `"opus"` {
		t.Fatal(s)
	}
}

// TestManyConcurrentClients runs 50 participants that chat and co-edit the
// prompt at once, each keeping its own replica fed by the event stream.
func TestManyConcurrentClients(t *testing.T) {
	const clients = 50
	const edits = 8
	env := newEnv(t, func(o *Options) { o.MaxParticipants = clients + 1; o.GCInterval = 50 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type peer struct {
		c    *Client
		id   string
		mu   sync.Mutex
		text *Text
		set  *ORSet
		chat atomic.Int64
	}
	peers := make([]*peer, clients)
	for i := range peers {
		role := RoleEditor
		if i%5 == 0 {
			role = RoleViewer
		}
		c := env.invite(fmt.Sprint("user", i), role)
		info, err := c.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		peers[i] = &peer{c: c, id: info.You.ID}
	}
	ready := make(chan struct{}, clients)
	var runWG sync.WaitGroup
	for _, p := range peers {
		runWG.Add(1)
		go func() {
			defer runWG.Done()
			signalled := false
			_ = p.c.Run(ctx, func(ev Event) error {
				p.mu.Lock()
				defer p.mu.Unlock()
				switch ev.Type {
				case EvWelcome:
					var w Welcome
					if err := json.Unmarshal(ev.Data, &w); err != nil {
						return err
					}
					if !w.Resumed {
						var err error
						if p.text, err = RestoreText(p.id, *w.Prompt); err != nil {
							return err
						}
						if p.set, err = RestoreORSet(p.id, *w.Context); err != nil {
							return err
						}
					}
					if !signalled {
						signalled = true
						ready <- struct{}{}
					}
				case EvOps:
					var oe OpsEvent
					_ = json.Unmarshal(ev.Data, &oe)
					ops, err := decodeOps(oe.Doc, oe.Ops)
					if err != nil {
						return err
					}
					switch o := ops.(type) {
					case []TextOp:
						_, err = p.text.Apply(o...)
					case []SetOp:
						_, err = p.set.Apply(o...)
					}
					return err
				case EvChat:
					p.chat.Add(1)
				}
				return nil
			})
		}()
	}
	for range clients {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("clients did not connect")
		}
	}
	var wg sync.WaitGroup
	var forbidden atomic.Int64
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 77))
			for k := range edits {
				if _, err := p.c.Send(ctx, MsgChat, ChatPost{Text: fmt.Sprintf("%d-%d", i, k)}); err != nil {
					t.Error(err)
					return
				}
				p.mu.Lock()
				viewer := i%5 == 0
				var op TextOp
				var sop SetOp
				var err error
				if viewer {
					// Viewers probe with a throwaway replica; the server
					// must refuse and their real replica stays clean.
					scratch := NewText(p.id)
					op, err = scratch.Insert(0, "nope")
					if err == nil {
						sop, err = NewORSet(p.id).Add("nope")
					}
				} else if p.text.Len() > 0 && rng.IntN(4) == 0 {
					op, err = p.text.Delete(rng.IntN(p.text.Len()), 1)
				} else {
					op, err = p.text.Insert(rng.IntN(p.text.Len()+1), fmt.Sprintf("<%d>", i))
				}
				if err == nil && !viewer {
					sop, err = p.set.Add(fmt.Sprintf("file%d.go", rng.IntN(10)))
				}
				vv := p.text.VersionVector()
				p.mu.Unlock()
				if err != nil {
					t.Error(err)
					return
				}
				_, err = p.c.Send(ctx, MsgOps, OpsMessage{Doc: DocPrompt, Ops: mustJSON([]TextOp{op}), VV: vv})
				if _, err2 := p.c.Send(ctx, MsgOps, OpsMessage{Doc: DocContext, Ops: mustJSON([]SetOp{sop})}); err == nil {
					err = err2
				}
				if viewer {
					if apiCode(err) != "forbidden" {
						t.Errorf("viewer edit: %v", err)
						return
					}
					forbidden.Add(1)
				} else if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if forbidden.Load() == 0 {
		t.Fatal("viewers were never refused")
	}
	want, _ := env.room.Prompt(ctx)
	wantSet, _ := env.room.ContextItems(ctx)
	totalChat := int64(clients * edits)
	for _, p := range peers {
		waitUntil(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.text != nil && p.text.String() == want && strings.Join(p.set.Elements(), ",") == strings.Join(wantSet, ",") && p.chat.Load() >= totalChat
		})
	}
	cancel()
	runWG.Wait()
	st, _ := env.room.Stats(context.Background())
	if st.Prompt.Pending != 0 {
		t.Fatalf("server holds pending ops: %+v", st.Prompt)
	}
}

func TestRoomGarbageCollection(t *testing.T) {
	env := newEnv(t, func(o *Options) { o.GCInterval = 30 * time.Millisecond })
	ctx := context.Background()
	if err := env.room.SetPrompt(ctx, "hello world"); err != nil {
		t.Fatal(err)
	}
	if err := env.room.SetPrompt(ctx, "bye"); err != nil {
		t.Fatal(err)
	}
	// The owner has a report only once it receives state: before that the
	// tombstones are collectable at once.
	waitUntil(t, func() bool {
		st, _ := env.room.Stats(ctx)
		return st.Prompt.Tombstones == 0 && st.Prompt.LogOps == 0
	})
	// With an editor whose report lags, collection waits.
	_ = collect(t, env.owner, 0, false) // reports the current state
	if err := env.room.SetPrompt(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	st, _ := env.room.Stats(ctx)
	if st.Prompt.Tombstones == 0 {
		t.Fatal("collected tombstones the owner has not acknowledged")
	}
	var vv VersionVector
	_ = env.room.exec(ctx, func() { vv = env.room.prompt.VersionVector() })
	if _, err := env.owner.Send(ctx, MsgHeartbeat, Heartbeat{VV: map[string]VersionVector{DocPrompt: vv}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		st, _ := env.room.Stats(ctx)
		return st.Prompt.Tombstones == 0
	})
}

func TestRecordingAndReplay(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	env := newEnv(t, func(o *Options) {
		o.NewRecorder = func(id string) (Recorder, error) {
			return NewLogRecorder(lockedWriter{&mu, &buf}, id), nil
		}
	})
	ctx := context.Background()
	for i := range 5 {
		if _, err := env.owner.Send(ctx, MsgChat, ChatPost{Text: fmt.Sprint("msg", i), Attachments: []Attachment{{Name: "b.bin", Data: []byte{0, 1, 2, 255, '\n'}}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	env.room.Close("test over")
	<-env.room.Done()
	mu.Lock()
	data := bytes.Clone(buf.Bytes())
	mu.Unlock()

	var slept time.Duration
	rp := NewReplayer(bytes.NewReader(data), ReplayOptions{Speed: 2, Verify: true, Sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil }})
	var types []string
	var first, last time.Time
	for rec, err := range rp.All(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if first.IsZero() {
			first = rec.Time
		}
		last = rec.Time
		types = append(types, rec.Type)
		if rec.Type == EvChat {
			var m ChatMessage
			_ = json.Unmarshal(rec.Data, &m)
			if !bytes.Equal(m.Attachments[0].Data, []byte{0, 1, 2, 255, '\n'}) {
				t.Fatal("binary attachment corrupted")
			}
		}
	}
	if n := strings.Count(strings.Join(types, " "), EvChat); n != 5 || types[len(types)-1] != EvClosed {
		t.Fatalf("recorded %v", types)
	}
	if want := last.Sub(first) / 2; slept < want-time.Millisecond || slept > want+time.Millisecond {
		t.Fatalf("slept %v, want about %v at 2x", slept, want)
	}
	// Any tampering breaks the chain.
	tampered := bytes.Replace(data, []byte("msg3"), []byte("msgX"), 1)
	rp = NewReplayer(bytes.NewReader(tampered), ReplayOptions{Verify: true})
	var terr error
	for _, err := range rp.All(ctx) {
		terr = err
	}
	if !errors.Is(terr, ErrTampered) {
		t.Fatalf("tampering not detected: %v", terr)
	}
	// Dropping a line is detected too.
	lines := bytes.SplitAfter(data, []byte("\n"))
	dropped := bytes.Join(append(lines[:1:1], lines[2:]...), nil)
	rp = NewReplayer(bytes.NewReader(dropped), ReplayOptions{Verify: true})
	terr = nil
	for _, err := range rp.All(ctx) {
		terr = err
	}
	if !errors.Is(terr, ErrTampered) {
		t.Fatalf("removal not detected: %v", terr)
	}
}

func TestReplayerPauseAndFileRecorder(t *testing.T) {
	path := t.TempDir() + "/room.ndjson"
	rec, err := CreateFileRecorder(path, "r1")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_800_000_000, 0).UTC()
	for i := range 3 {
		if err := rec.Record(Event{Seq: uint64(i + 1), Type: EvChat, Time: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = rec.Close()
	// Reopening continues the hash chain.
	rec, err = CreateFileRecorder(path, "r1")
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Record(Event{Seq: 4, Type: EvChat, Time: base.Add(10 * time.Second)})
	_ = rec.Close()
	data, _ := readFile(path)
	var waits []time.Duration
	rp := NewReplayer(bytes.NewReader(data), ReplayOptions{Speed: 1, MaxGap: 3 * time.Second, Verify: true,
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }})
	rp.Pause()
	got := make(chan Record, 4)
	go func() {
		for r, err := range rp.All(context.Background()) {
			if err != nil {
				t.Error(err)
				return
			}
			got <- r
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("paused replayer produced a record")
	case <-time.After(50 * time.Millisecond):
	}
	rp.Resume()
	n := 0
	for range got {
		n++
	}
	if n != 4 || len(waits) != 3 || waits[0] != time.Second || waits[2] != 3*time.Second {
		t.Fatalf("records %d waits %v", n, waits)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func readFile(path string) ([]byte, error) { return osReadFile(path) }

func TestRingLog(t *testing.T) {
	r := newRingLog(3)
	if _, ok := r.since(0); !ok {
		t.Fatal("empty log, seq 0")
	}
	for i := uint64(1); i <= 5; i++ {
		r.append(i, []byte{byte(i)})
	}
	f, ok := r.since(2)
	if !ok || len(f) != 3 || f[0][0] != 3 || f[2][0] != 5 {
		t.Fatal(f, ok)
	}
	if _, ok := r.since(1); ok {
		t.Fatal("evicted range reported available")
	}
	if f, ok := r.since(5); !ok || len(f) != 0 {
		t.Fatal("up to date")
	}
	if _, ok := r.since(9); ok {
		t.Fatal("future id accepted")
	}
}
