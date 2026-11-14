package collab

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// DefaultMaxDrift is how far ahead of local physical time a remote
// timestamp may be before Clock.Update refuses it.
const DefaultMaxDrift = time.Minute

// ErrClockDrift reports a remote timestamp too far ahead of local time.
var ErrClockDrift = errors.New("collab: remote timestamp exceeds the allowed clock drift")

// Timestamp is a hybrid logical clock reading: physical wall time in Unix
// nanoseconds, a logical counter that orders events within the same wall
// reading, and the issuing node to break the remaining ties. Timestamps
// from one Clock are strictly increasing, and Compare is a total order.
type Timestamp struct {
	Wall    int64  `json:"w"`
	Logical uint32 `json:"l,omitempty"`
	Node    string `json:"n"`
}

// IsZero reports whether t is the zero timestamp.
func (t Timestamp) IsZero() bool { return t.Wall == 0 && t.Logical == 0 && t.Node == "" }

// Compare orders by wall time, then logical counter, then node.
func (t Timestamp) Compare(o Timestamp) int {
	switch {
	case t.Wall < o.Wall:
		return -1
	case t.Wall > o.Wall:
		return 1
	case t.Logical < o.Logical:
		return -1
	case t.Logical > o.Logical:
		return 1
	default:
		return strings.Compare(t.Node, o.Node)
	}
}

// Before reports whether t orders before o.
func (t Timestamp) Before(o Timestamp) bool { return t.Compare(o) < 0 }

// Time returns the physical component.
func (t Timestamp) Time() time.Time { return time.Unix(0, t.Wall) }

// String formats t as wall.logical@node.
func (t Timestamp) String() string { return fmt.Sprintf("%d.%d@%s", t.Wall, t.Logical, t.Node) }

// Clock is a hybrid logical clock (Kulkarni et al., 2014). It tracks
// physical time closely, never runs backwards even when the system clock
// does, and advances past every timestamp it observes, so a timestamp it
// issues after observing another is always greater. It is safe for
// concurrent use.
type Clock struct {
	mu       sync.Mutex
	node     string
	now      func() time.Time
	maxDrift time.Duration
	wall     int64
	logical  uint32
}

// ClockOption configures a Clock.
type ClockOption func(*Clock)

// WithPhysicalClock replaces time.Now, for tests and simulations.
func WithPhysicalClock(now func() time.Time) ClockOption { return func(c *Clock) { c.now = now } }

// WithMaxDrift sets the drift bound used by Update. Zero or negative
// disables the check.
func WithMaxDrift(d time.Duration) ClockOption { return func(c *Clock) { c.maxDrift = d } }

// NewClock returns a clock issuing timestamps for node.
func NewClock(node string, opts ...ClockOption) *Clock {
	c := &Clock{node: node, now: time.Now, maxDrift: DefaultMaxDrift}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Node returns the clock's node id.
func (c *Clock) Node() string { return c.node }

// Last returns the most recent timestamp issued or observed.
func (c *Clock) Last() Timestamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Timestamp{Wall: c.wall, Logical: c.logical, Node: c.node}
}

// Now issues a new timestamp for a local event.
func (c *Clock) Now() Timestamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.now().UnixNano()
	if pt > c.wall {
		c.wall, c.logical = pt, 0
	} else {
		c.tick()
	}
	return Timestamp{Wall: c.wall, Logical: c.logical, Node: c.node}
}

// Update merges a remote timestamp and issues a timestamp for its receipt,
// greater than both the remote one and anything this clock issued before.
// A remote wall time more than the drift bound ahead of local physical time
// is refused with ErrClockDrift and leaves the clock unchanged, so a peer
// with a broken clock cannot drag everyone into the future.
func (c *Clock) Update(remote Timestamp) (Timestamp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.now().UnixNano()
	if c.maxDrift > 0 && remote.Wall > pt && time.Duration(remote.Wall-pt) > c.maxDrift {
		return Timestamp{}, fmt.Errorf("%w: %v ahead", ErrClockDrift, time.Duration(remote.Wall-pt))
	}
	c.merge(pt, remote)
	return Timestamp{Wall: c.wall, Logical: c.logical, Node: c.node}, nil
}

// Observe merges a remote timestamp when it is within the drift bound and
// ignores it otherwise. It never fails; replicas use it when applying
// operations that must be accepted for convergence regardless of clocks.
func (c *Clock) Observe(remote Timestamp) {
	_, _ = c.Update(remote)
}

func (c *Clock) merge(pt int64, m Timestamp) {
	wall := max(c.wall, m.Wall, pt)
	switch {
	case wall == c.wall && wall == m.Wall:
		c.logical = max(c.logical, m.Logical)
		c.wall = wall
		c.tick()
	case wall == c.wall:
		c.tick()
	case wall == m.Wall:
		c.wall, c.logical = wall, m.Logical
		c.tick()
	default:
		c.wall, c.logical = wall, 0
	}
}

// tick advances the logical counter, carrying into the wall component on
// overflow so ordering is preserved.
func (c *Clock) tick() {
	if c.logical == math.MaxUint32 {
		c.wall++
		c.logical = 0
		return
	}
	c.logical++
}
