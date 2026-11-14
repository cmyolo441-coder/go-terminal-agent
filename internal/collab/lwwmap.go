package collab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// MaxValueBytes bounds an LWW-Map value.
const MaxValueBytes = 64 << 10

// MapOp is one replicated LWW-Map write: a value, or a deletion, stamped
// with a hybrid logical clock.
type MapOp struct {
	ID      Dot             `json:"id"`
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"v,omitempty"`
	Deleted bool            `json:"del,omitempty"`
	TS      Timestamp       `json:"ts"`
}

func (op MapOp) opDot() Dot { return op.ID }

// Validate checks the operation's shape.
func (op MapOp) Validate() error {
	if op.ID.Replica == "" || op.ID.Seq == 0 {
		return fmt.Errorf("%w: map operation without id", ErrInvalidOp)
	}
	if op.Key == "" || len(op.Key) > MaxElementBytes || !utf8.ValidString(op.Key) {
		return fmt.Errorf("%w: map key must be 1..%d bytes of UTF-8", ErrInvalidOp, MaxElementBytes)
	}
	if op.TS.Wall <= 0 || op.TS.Node == "" {
		return fmt.Errorf("%w: map operation without timestamp", ErrInvalidOp)
	}
	if op.Deleted {
		if len(op.Value) != 0 {
			return fmt.Errorf("%w: deletion with a value", ErrInvalidOp)
		}
		return nil
	}
	if len(op.Value) == 0 || len(op.Value) > MaxValueBytes || !json.Valid(op.Value) {
		return fmt.Errorf("%w: map value must be 1..%d bytes of JSON", ErrInvalidOp, MaxValueBytes)
	}
	return nil
}

type lwwEntry struct {
	value   json.RawMessage
	ts      Timestamp
	deleted bool
}

// LWWMap is a last-writer-wins map from strings to JSON values. Each key
// keeps the write with the greatest hybrid-logical-clock timestamp, so
// later writes win and concurrent ones resolve the same way everywhere.
// Deletions are timestamped tombstones. It is not safe for concurrent use.
type LWWMap struct {
	replica string
	clock   *Clock
	log     opLog[MapOp]
	entries map[string]lwwEntry
}

// NewLWWMap returns an empty map; clock nil uses a new real-time clock.
func NewLWWMap(replica string, clock *Clock) *LWWMap {
	if clock == nil {
		clock = NewClock(replica)
	}
	return &LWWMap{replica: replica, clock: clock, log: newOpLog[MapOp](0), entries: map[string]lwwEntry{}}
}

// Clock returns the map's clock.
func (m *LWWMap) Clock() *Clock { return m.clock }

// Set writes key = value (marshalled to JSON).
func (m *LWWMap) Set(key string, value any) (MapOp, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return MapOp{}, fmt.Errorf("collab: encode value for %q: %w", key, err)
	}
	return m.local(MapOp{Key: key, Value: compactJSON(raw)})
}

// Delete removes key.
func (m *LWWMap) Delete(key string) (MapOp, error) {
	return m.local(MapOp{Key: key, Deleted: true})
}

func (m *LWWMap) local(op MapOp) (MapOp, error) {
	op.ID = m.log.next(m.replica)
	op.TS = m.clock.Now()
	if err := op.Validate(); err != nil {
		return MapOp{}, err
	}
	m.apply(op)
	m.log.commit(op)
	return op, nil
}

// Get returns key's value.
func (m *LWWMap) Get(key string) (json.RawMessage, bool) {
	e, ok := m.entries[key]
	if !ok || e.deleted {
		return nil, false
	}
	return e.value, true
}

// Keys returns the live keys, sorted.
func (m *LWWMap) Keys() []string {
	var out []string
	for k, e := range m.entries {
		if !e.deleted {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// Entries returns the live key-value pairs.
func (m *LWWMap) Entries() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, e := range m.entries {
		if !e.deleted {
			out[k] = e.value
		}
	}
	return out
}

// VersionVector returns a copy of the applied-operations vector.
func (m *LWWMap) VersionVector() VersionVector { return m.log.vv.Clone() }

// Apply integrates remote operations; see Text.Apply.
func (m *LWWMap) Apply(ops ...MapOp) ([]MapOp, error) {
	for _, op := range ops {
		if err := op.Validate(); err != nil {
			return nil, err
		}
	}
	return m.log.ingest(ops, func(MapOp) bool { return true }, func(op MapOp) error {
		m.clock.Observe(op.TS)
		m.apply(op)
		return nil
	})
}

// Delta returns the operations a replica at remote is missing.
func (m *LWWMap) Delta(remote VersionVector) ([]MapOp, error) { return m.log.delta(remote) }

// Compact drops logged operations covered by frontier.
func (m *LWWMap) Compact(frontier VersionVector) int { return m.log.compact(frontier) }

// DropPending discards buffered operations from replica.
func (m *LWWMap) DropPending(replica string) int { return m.log.dropPending(replica) }

func (m *LWWMap) apply(op MapOp) {
	cur, ok := m.entries[op.Key]
	if ok && op.TS.Compare(cur.ts) <= 0 {
		return
	}
	m.entries[op.Key] = lwwEntry{value: op.Value, ts: op.TS, deleted: op.Deleted}
}

func compactJSON(raw []byte) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

// MapEntryState is one entry in an LWWMapSnapshot.
type MapEntryState struct {
	Value   json.RawMessage `json:"v,omitempty"`
	TS      Timestamp       `json:"ts"`
	Deleted bool            `json:"del,omitempty"`
}

// LWWMapSnapshot is the full state of an LWWMap.
type LWWMapSnapshot struct {
	VV      VersionVector            `json:"vv"`
	Base    VersionVector            `json:"base,omitempty"`
	Entries map[string]MapEntryState `json:"entries"`
	Log     []MapOp                  `json:"log,omitempty"`
}

// Snapshot captures the state; withLog includes the operation log.
func (m *LWWMap) Snapshot(withLog bool) LWWMapSnapshot {
	s := LWWMapSnapshot{VV: m.log.vv.Clone(), Entries: make(map[string]MapEntryState, len(m.entries))}
	if withLog {
		s.Base, s.Log = m.log.base.Clone(), slices.Clone(m.log.applied)
	} else {
		s.Base = m.log.vv.Clone()
	}
	for _, k := range slices.Sorted(maps.Keys(m.entries)) {
		e := m.entries[k]
		s.Entries[k] = MapEntryState{Value: e.value, TS: e.ts, Deleted: e.deleted}
	}
	return s
}

// RestoreLWWMap builds a replica from a snapshot.
func RestoreLWWMap(replica string, clock *Clock, s LWWMapSnapshot) (*LWWMap, error) {
	m := NewLWWMap(replica, clock)
	base := s.Base
	if base == nil {
		base = s.VV
	}
	if err := m.log.restore(s.VV, base, s.Log); err != nil {
		return nil, err
	}
	for _, op := range s.Log {
		if err := op.Validate(); err != nil {
			return nil, err
		}
	}
	for k, e := range s.Entries {
		probe := MapOp{ID: Dot{Replica: "snapshot", Seq: 1}, Key: k, Value: e.Value, Deleted: e.Deleted, TS: e.TS}
		if err := probe.Validate(); err != nil {
			return nil, err
		}
		m.clock.Observe(e.TS)
		m.entries[k] = lwwEntry{value: e.Value, ts: e.TS, deleted: e.Deleted}
	}
	return m, nil
}
