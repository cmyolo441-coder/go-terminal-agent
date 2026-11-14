package collab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Role is a participant's authority in a room. Roles are ordered: an owner
// can do everything an editor can, and an editor everything a viewer can.
type Role uint8

// Roles.
const (
	RoleViewer Role = iota + 1
	RoleEditor
	RoleOwner
)

// ParseRole parses "owner", "editor" or "viewer".
func ParseRole(s string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "owner":
		return RoleOwner, nil
	case "editor":
		return RoleEditor, nil
	case "viewer":
		return RoleViewer, nil
	}
	return 0, fmt.Errorf("collab: unknown role %q (want owner, editor or viewer)", s)
}

// Valid reports whether r is a defined role.
func (r Role) Valid() bool { return r >= RoleViewer && r <= RoleOwner }

// AtLeast reports whether r ranks at or above o.
func (r Role) AtLeast(o Role) bool { return r.Valid() && r >= o }

func (r Role) String() string {
	switch r {
	case RoleOwner:
		return "owner"
	case RoleEditor:
		return "editor"
	case RoleViewer:
		return "viewer"
	}
	return fmt.Sprintf("role(%d)", uint8(r))
}

// MarshalText implements encoding.TextMarshaler.
func (r Role) MarshalText() ([]byte, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("collab: invalid role %d", uint8(r))
	}
	return []byte(r.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *Role) UnmarshalText(b []byte) error {
	v, err := ParseRole(string(b))
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// Upstream (client to server) message types.
const (
	MsgHeartbeat = "heartbeat"
	MsgPresence  = "presence"
	MsgChat      = "chat"
	MsgOps       = "ops"
	MsgSync      = "sync"
	MsgVote      = "vote"
	MsgSubmit    = "submit"
	MsgSetRole   = "set_role"
	MsgKick      = "kick"
	MsgLeave     = "leave"
)

// Downstream (server to client) event types.
const (
	EvWelcome          = "welcome"
	EvJoined           = "participant_joined"
	EvLeft             = "participant_left"
	EvPresence         = "presence"
	EvChat             = "chat"
	EvOps              = "ops"
	EvSubmitted        = "prompt_submitted"
	EvApprovalRequest  = "approval_requested"
	EvApprovalVote     = "approval_vote"
	EvApprovalResolved = "approval_resolved"
	EvRoleChanged      = "role_changed"
	EvAgent            = "agent"
	EvClosed           = "room_closed"
	EvDisconnect       = "disconnect"
)

// Shared document names.
const (
	DocPrompt   = "prompt"
	DocContext  = "context"
	DocSettings = "settings"
)

// Protocol limits.
const (
	MaxMessageBytes    = 1 << 20
	MaxChatRunes       = 8000
	MaxAttachments     = 4
	MaxAttachmentBytes = 192 << 10
	MaxNameRunes       = 64
	MaxOpsPerMessage   = 4096
)

// ErrBadMessage reports a malformed upstream message.
var ErrBadMessage = errors.New("collab: bad message")

// ClientMessage is the upstream envelope. Seq, when non-zero, must
// increase per participant; a message whose Seq is not above the last one
// accepted is acknowledged as a duplicate without effect, which makes
// retries after a lost response safe.
type ClientMessage struct {
	Type string          `json:"type"`
	Seq  uint64          `json:"seq,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Heartbeat optionally reports the sender's version vector per document,
// which the server uses to compute the causal-stability frontier.
type Heartbeat struct {
	VV map[string]VersionVector `json:"vv,omitempty"`
}

// Selection is a cursor range in the prompt, in rune offsets.
type Selection struct {
	Anchor int `json:"anchor"`
	Head   int `json:"head"`
}

// PresenceUpdate changes a participant's awareness state. Nil fields are
// left unchanged.
type PresenceUpdate struct {
	Cursor    *int       `json:"cursor,omitempty"`
	Selection *Selection `json:"selection,omitempty"`
	Typing    *bool      `json:"typing,omitempty"`
	Status    string     `json:"status,omitempty"`
}

// Attachment is binary content attached to a chat message; Data travels
// base64-encoded in JSON.
type Attachment struct {
	Name string `json:"name"`
	MIME string `json:"mime,omitempty"`
	Data []byte `json:"data"`
}

// ChatPost is an upstream chat message.
type ChatPost struct {
	Text        string       `json:"text"`
	ReplyTo     uint64       `json:"reply_to,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// OpsMessage carries replicated operations for one document, with the
// sender's version vector of that document after applying them.
type OpsMessage struct {
	Doc string          `json:"doc"`
	Ops json.RawMessage `json:"ops"`
	VV  VersionVector   `json:"vv,omitempty"`
}

// SyncRequest asks for the operations of Doc missing from VV, or for a
// snapshot when Reset is set or the operations were compacted away.
type SyncRequest struct {
	Doc   string        `json:"doc"`
	VV    VersionVector `json:"vv,omitempty"`
	Reset bool          `json:"reset,omitempty"`
}

// VoteRequest is a vote on a pending approval.
type VoteRequest struct {
	ApprovalID string `json:"approval_id"`
	Approve    bool   `json:"approve"`
	Comment    string `json:"comment,omitempty"`
}

// SubmitRequest hands the shared prompt to the agent.
type SubmitRequest struct {
	// Expect, when set, must equal the prompt text, guarding against
	// submitting edits the submitter has not seen.
	Expect *string `json:"expect,omitempty"`
}

// SetRoleRequest changes a participant's role.
type SetRoleRequest struct {
	Participant string `json:"participant"`
	Role        Role   `json:"role"`
}

// KickRequest removes a participant from the room.
type KickRequest struct {
	Participant string `json:"participant"`
	Reason      string `json:"reason,omitempty"`
}

// strictUnmarshal decodes exactly one JSON value into v, rejecting unknown
// fields and trailing data.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON value")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func badf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadMessage, fmt.Sprintf(format, a...))
}

// DecodeClientMessage parses and validates an upstream message, returning
// the envelope and its typed payload (a pointer to one of the request
// types above, or nil for heartbeat and leave). It accepts nothing it
// cannot fully validate without room state.
func DecodeClientMessage(data []byte) (ClientMessage, any, error) {
	var m ClientMessage
	if len(data) > MaxMessageBytes {
		return m, nil, badf("message of %d bytes exceeds %d", len(data), MaxMessageBytes)
	}
	if err := strictUnmarshal(data, &m); err != nil {
		return m, nil, badf("%v", err)
	}
	empty := len(bytes.TrimSpace(m.Data)) == 0 || bytes.Equal(bytes.TrimSpace(m.Data), []byte("null"))
	decode := func(v any) error {
		if empty {
			return badf("%s needs data", m.Type)
		}
		if err := strictUnmarshal(m.Data, v); err != nil {
			return badf("%s data: %v", m.Type, err)
		}
		return nil
	}
	switch m.Type {
	case MsgHeartbeat, MsgLeave:
		if !empty {
			var hb Heartbeat
			if err := strictUnmarshal(m.Data, &hb); err != nil {
				return m, nil, badf("%s data: %v", m.Type, err)
			}
			for doc := range hb.VV {
				if !validDoc(doc) {
					return m, nil, badf("unknown document %q", doc)
				}
			}
			return m, &hb, nil
		}
		return m, nil, nil
	case MsgPresence:
		var p PresenceUpdate
		if err := decode(&p); err != nil {
			return m, nil, err
		}
		if p.Cursor != nil && *p.Cursor < 0 || p.Selection != nil && (p.Selection.Anchor < 0 || p.Selection.Head < 0) {
			return m, nil, badf("negative cursor position")
		}
		switch p.Status {
		case "", "active", "idle", "away":
		default:
			return m, nil, badf("unknown status %q", p.Status)
		}
		return m, &p, nil
	case MsgChat:
		var c ChatPost
		if err := decode(&c); err != nil {
			return m, nil, err
		}
		if !utf8.ValidString(c.Text) {
			return m, nil, badf("chat text is not UTF-8")
		}
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" && len(c.Attachments) == 0 {
			return m, nil, badf("empty chat message")
		}
		if n := utf8.RuneCountInString(c.Text); n > MaxChatRunes {
			return m, nil, badf("chat text of %d runes exceeds %d", n, MaxChatRunes)
		}
		if len(c.Attachments) > MaxAttachments {
			return m, nil, badf("more than %d attachments", MaxAttachments)
		}
		for _, a := range c.Attachments {
			if a.Name == "" || utf8.RuneCountInString(a.Name) > 255 || !utf8.ValidString(a.Name) || strings.ContainsAny(a.Name, "/\\\x00") {
				return m, nil, badf("bad attachment name")
			}
			if len(a.Data) > MaxAttachmentBytes {
				return m, nil, badf("attachment %q exceeds %d bytes", a.Name, MaxAttachmentBytes)
			}
			if len(a.MIME) > 127 || !utf8.ValidString(a.MIME) {
				return m, nil, badf("bad attachment MIME type")
			}
		}
		return m, &c, nil
	case MsgOps:
		var o OpsMessage
		if err := decode(&o); err != nil {
			return m, nil, err
		}
		if !validDoc(o.Doc) {
			return m, nil, badf("unknown document %q", o.Doc)
		}
		if _, err := decodeOps(o.Doc, o.Ops); err != nil {
			return m, nil, err
		}
		return m, &o, nil
	case MsgSync:
		var s SyncRequest
		if err := decode(&s); err != nil {
			return m, nil, err
		}
		if !validDoc(s.Doc) {
			return m, nil, badf("unknown document %q", s.Doc)
		}
		return m, &s, nil
	case MsgVote:
		var v VoteRequest
		if err := decode(&v); err != nil {
			return m, nil, err
		}
		if v.ApprovalID == "" || len(v.ApprovalID) > 128 {
			return m, nil, badf("vote needs an approval_id")
		}
		if utf8.RuneCountInString(v.Comment) > 1000 || !utf8.ValidString(v.Comment) {
			return m, nil, badf("vote comment too long")
		}
		return m, &v, nil
	case MsgSubmit:
		var s SubmitRequest
		if !empty {
			if err := decode(&s); err != nil {
				return m, nil, err
			}
		}
		return m, &s, nil
	case MsgSetRole:
		var s SetRoleRequest
		if err := decode(&s); err != nil {
			return m, nil, err
		}
		if s.Participant == "" || !s.Role.Valid() {
			return m, nil, badf("set_role needs a participant and a valid role")
		}
		return m, &s, nil
	case MsgKick:
		var k KickRequest
		if err := decode(&k); err != nil {
			return m, nil, err
		}
		if k.Participant == "" || utf8.RuneCountInString(k.Reason) > 500 || !utf8.ValidString(k.Reason) {
			return m, nil, badf("kick needs a participant")
		}
		return m, &k, nil
	case "":
		return m, nil, badf("missing type")
	}
	return m, nil, badf("unknown message type %q", m.Type)
}

func validDoc(d string) bool { return d == DocPrompt || d == DocContext || d == DocSettings }

// decodeOps decodes and validates a document's operation list, returning
// []TextOp, []SetOp or []MapOp.
func decodeOps(doc string, raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, badf("ops: missing operations")
	}
	check := func(n int) error {
		if n == 0 || n > MaxOpsPerMessage {
			return badf("ops: need 1..%d operations, got %d", MaxOpsPerMessage, n)
		}
		return nil
	}
	switch doc {
	case DocPrompt:
		var ops []TextOp
		if err := strictUnmarshal(raw, &ops); err != nil {
			return nil, badf("ops: %v", err)
		}
		if err := check(len(ops)); err != nil {
			return nil, err
		}
		for _, op := range ops {
			if err := op.Validate(); err != nil {
				return nil, badf("ops: %v", err)
			}
		}
		return ops, nil
	case DocContext:
		var ops []SetOp
		if err := strictUnmarshal(raw, &ops); err != nil {
			return nil, badf("ops: %v", err)
		}
		if err := check(len(ops)); err != nil {
			return nil, err
		}
		for _, op := range ops {
			if err := op.Validate(); err != nil {
				return nil, badf("ops: %v", err)
			}
		}
		return ops, nil
	case DocSettings:
		var ops []MapOp
		if err := strictUnmarshal(raw, &ops); err != nil {
			return nil, badf("ops: %v", err)
		}
		if err := check(len(ops)); err != nil {
			return nil, err
		}
		for _, op := range ops {
			if err := op.Validate(); err != nil {
				return nil, badf("ops: %v", err)
			}
		}
		return ops, nil
	}
	return nil, badf("unknown document %q", doc)
}

// Event is the downstream envelope, sent as the data of an SSE event. Seq
// is the room-wide event number (also the SSE id); ephemeral events such
// as presence and per-connection events carry no Seq and are not replayed.
type Event struct {
	Seq  uint64          `json:"seq,omitempty"`
	Type string          `json:"type"`
	Time time.Time       `json:"time"`
	From string          `json:"from,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ParticipantInfo describes a participant to others.
type ParticipantInfo struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Role     Role       `json:"role"`
	Online   bool       `json:"online"`
	Status   string     `json:"status"`
	Cursor   *int       `json:"cursor,omitempty"`
	Sel      *Selection `json:"selection,omitempty"`
	Typing   bool       `json:"typing,omitempty"`
	LastSeen time.Time  `json:"last_seen,omitzero"`
	Joined   time.Time  `json:"joined"`
}

// ChatMessage is a chat event's payload.
type ChatMessage struct {
	ID          uint64       `json:"id"`
	From        string       `json:"from"`
	Name        string       `json:"name"`
	Text        string       `json:"text"`
	ReplyTo     uint64       `json:"reply_to,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Time        time.Time    `json:"time"`
}

// OpsEvent is an ops event's payload: operations newly applied by the
// server, in causal order.
type OpsEvent struct {
	Doc string          `json:"doc"`
	Ops json.RawMessage `json:"ops"`
}

// LeftEvent is a participant_left payload.
type LeftEvent struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// RoleChange is a role_changed payload.
type RoleChange struct {
	Participant string `json:"participant"`
	Role        Role   `json:"role"`
	By          string `json:"by"`
}

// AgentEvent is an agent payload published by the host process.
type AgentEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Submission is a prompt_submitted payload.
type Submission struct {
	By   string `json:"by"`
	Text string `json:"text"`
}

// DisconnectEvent tells a client why the server ended its stream.
type DisconnectEvent struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Disconnect reasons.
const (
	ReasonSlowConsumer = "slow_consumer"
	ReasonKicked       = "kicked"
	ReasonRoomClosed   = "room_closed"
	ReasonLeft         = "left"
	ReasonTimeout      = "timeout"
	ReasonReplaced     = "too_many_streams"
)

// RoomInfo summarises a room.
type RoomInfo struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Created      time.Time    `json:"created"`
	Participants int          `json:"participants"`
	Online       int          `json:"online"`
	Seq          uint64       `json:"seq"`
	Policy       QuorumPolicy `json:"policy"`
}

// Welcome is the first event of every stream. When Resumed is set the
// server replays every event after the client's Last-Event-ID and the
// document snapshots are omitted; otherwise the snapshots are the state as
// of Seq and the client must reset.
type Welcome struct {
	Room         RoomInfo          `json:"room"`
	You          ParticipantInfo   `json:"you"`
	Seq          uint64            `json:"seq"`
	Resumed      bool              `json:"resumed"`
	Participants []ParticipantInfo `json:"participants"`
	Chat         []ChatMessage     `json:"chat,omitempty"`
	Approvals    []ApprovalView    `json:"approvals,omitempty"`
	Prompt       *TextSnapshot     `json:"prompt,omitempty"`
	Context      *ORSetSnapshot    `json:"context,omitempty"`
	Settings     *LWWMapSnapshot   `json:"settings,omitempty"`
}

// Reply is the body of a successful upstream POST.
type Reply struct {
	OK        bool            `json:"ok"`
	Duplicate bool            `json:"duplicate,omitempty"`
	Seq       uint64          `json:"seq,omitempty"`
	Applied   int             `json:"applied,omitempty"`
	Pending   int             `json:"pending,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// SyncReply is the Result of a sync request.
type SyncReply struct {
	Doc      string          `json:"doc"`
	Ops      json.RawMessage `json:"ops,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	VV       VersionVector   `json:"vv"`
}

// mustJSON marshals v; values here are all JSON-safe by construction.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": "encode: " + err.Error()})
	}
	return b
}
