package sentinel

import (
	"fmt"
	"time"
)

// Kind classifies an Event.
type Kind uint8

// Event kinds.
const (
	KindUnknown Kind = iota
	// KindToolCall is a tool call about to run that has no more specific kind.
	KindToolCall
	// KindToolResult is the outcome of a tool call.
	KindToolResult
	// KindLLMRequest is a model call about to be made.
	KindLLMRequest
	// KindLLMResponse is a finished model call, with Tokens and Cost.
	KindLLMResponse
	// KindFileWrite is a file about to be created or changed (Target is the path).
	KindFileWrite
	// KindFileRead is a file about to be read (Target is the path).
	KindFileRead
	// KindFileDelete is a file about to be removed (Target is the path).
	KindFileDelete
	// KindExec is a shell command about to run (Target is the command line).
	KindExec
	// KindNet is a network request about to be made (Target is the URL or host).
	KindNet
	numKinds
)

var kindNames = [numKinds]string{
	"unknown", "tool_call", "tool_result", "llm_request", "llm_response",
	"file_write", "file_read", "file_delete", "exec", "net",
}

// String returns the snake_case name of k.
func (k Kind) String() string {
	if k < numKinds {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// MarshalText implements encoding.TextMarshaler.
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *Kind) UnmarshalText(b []byte) error {
	v, err := ParseKind(string(b))
	if err != nil {
		return err
	}
	*k = v
	return nil
}

// ParseKind parses a kind name as produced by Kind.String.
func ParseKind(s string) (Kind, error) {
	for i, n := range kindNames {
		if n == s {
			return Kind(i), nil
		}
	}
	return KindUnknown, fmt.Errorf("sentinel: unknown event kind %q", s)
}

// IsAction reports whether k is something the agent is about to do to the
// outside world (a tool call, file operation, command or network request).
func (k Kind) IsAction() bool { return ActionKinds.Has(k) }

// gated reports whether events of kind k receive a blocking verdict.
func (k Kind) gated() bool { return k.IsAction() || k == KindLLMRequest }

// isResult reports whether k reports an outcome.
func (k Kind) isResult() bool { return k == KindToolResult || k == KindLLMResponse }

// KindSet is a set of kinds.
type KindSet uint32

// Kinds builds a KindSet.
func Kinds(ks ...Kind) KindSet {
	var s KindSet
	for _, k := range ks {
		s |= 1 << k
	}
	return s
}

// Has reports whether k is in s.
func (s KindSet) Has(k Kind) bool { return s&(1<<k) != 0 }

// ActionKinds are the kinds IsAction reports.
var ActionKinds = Kinds(KindToolCall, KindFileWrite, KindFileRead, KindFileDelete, KindExec, KindNet)

// EventFlags carry extra facts about an event.
type EventFlags uint8

const (
	// FlagOutsideRoot marks a file operation outside the workspace.
	FlagOutsideRoot EventFlags = 1 << iota
)

// Event is one observable thing the agent did or is about to do.
//
// Callers emit exactly one pre-execution event per tool call, using the most
// specific kind (exec, file_write, file_read, file_delete, net, else
// tool_call), and one KindToolResult afterwards.
type Event struct {
	// Time is when the event happened; zero means the sentinel's clock.
	Time time.Time
	Kind Kind
	// Session groups events; detectors keep separate state per session.
	Session string
	// Tool is the tool name (or model name for LLM events).
	Tool string
	// Target is the command line, path or URL the event acts on.
	Target string
	// Bytes is the size written, read or transferred, if known.
	Bytes int64
	// Tokens is the tokens used by an LLM response.
	Tokens int64
	// Cost is the USD cost of an LLM response.
	Cost float64
	// Err is the failure of a result event; nil means success.
	Err error
	// Digest identifies a file's content after a write (any stable 64-bit
	// hash). Zero means unknown; the sentinel then hashes Payload if set.
	Digest uint64
	// Payload is (a prefix of) the content written. It is inspected for
	// entropy and never retained.
	Payload []byte
	Flags   EventFlags
}

// record is the compact copy of an Event kept in a session's ring. It holds
// no Payload so evidence never pins written content in memory.
type record struct {
	seq    uint64
	at     int64
	kind   Kind
	flags  EventFlags
	tool   string
	target string
	bytes  int64
	tokens int64
	cost   float64
	err    error
}

// EventRecord is an event as kept in incident evidence.
type EventRecord struct {
	Seq    uint64    `json:"seq"`
	Time   time.Time `json:"time"`
	Kind   Kind      `json:"kind"`
	Tool   string    `json:"tool,omitempty"`
	Target string    `json:"target,omitempty"`
	Bytes  int64     `json:"bytes,omitempty"`
	Tokens int64     `json:"tokens,omitempty"`
	Cost   float64   `json:"cost_usd,omitempty"`
	Err    string    `json:"error,omitempty"`
	// Matched marks the events that matched the detector's rule.
	Matched bool `json:"matched,omitempty"`
}

func (r *record) export(redact func(string) string, maxTarget int) EventRecord {
	target := r.target
	if redact != nil {
		target = redact(target)
	}
	target = truncateBytes(target, maxTarget)
	er := EventRecord{
		Seq: r.seq, Time: time.Unix(0, r.at).UTC(), Kind: r.kind, Tool: r.tool, Target: target,
		Bytes: r.bytes, Tokens: r.tokens, Cost: r.cost,
	}
	if r.err != nil {
		msg := r.err.Error()
		if redact != nil {
			msg = redact(msg)
		}
		er.Err = truncateBytes(msg, maxTarget)
	}
	return er
}

// truncateBytes cuts s to at most n bytes on a UTF-8 boundary, marking the cut.
func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
