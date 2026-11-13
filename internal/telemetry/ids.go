package telemetry

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"sync"
)

// TraceID is a 16-byte W3C trace id.
type TraceID [16]byte

// SpanID is an 8-byte W3C span id.
type SpanID [8]byte

// IsValid reports whether the id is non-zero, as W3C Trace Context requires.
func (t TraceID) IsValid() bool { return t != TraceID{} }

// IsValid reports whether the id is non-zero, as W3C Trace Context requires.
func (s SpanID) IsValid() bool { return s != SpanID{} }

// String renders the id as 32 lower-case hex digits.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// String renders the id as 16 lower-case hex digits.
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

var errBadID = errors.New("telemetry: invalid id")

// ParseTraceID decodes 32 lower-case hex digits. All-zero ids are rejected.
func ParseTraceID(s string) (TraceID, error) {
	var t TraceID
	if err := decodeLowerHex(t[:], s); err != nil || !t.IsValid() {
		return TraceID{}, errBadID
	}
	return t, nil
}

// ParseSpanID decodes 16 lower-case hex digits. All-zero ids are rejected.
func ParseSpanID(s string) (SpanID, error) {
	var id SpanID
	if err := decodeLowerHex(id[:], s); err != nil || !id.IsValid() {
		return SpanID{}, errBadID
	}
	return id, nil
}

// decodeLowerHex decodes exactly len(dst)*2 lower-case hex digits; W3C
// Trace Context forbids upper case.
func decodeLowerHex(dst []byte, s string) error {
	if len(s) != len(dst)*2 {
		return errBadID
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errBadID
		}
	}
	_, err := hex.Decode(dst, []byte(s))
	return err
}

// idGenerator produces random ids from a ChaCha8 stream seeded from the
// operating system, so id generation never blocks on crypto/rand.
type idGenerator struct {
	mu  sync.Mutex
	rng *rand.ChaCha8
}

func newIDGenerator() *idGenerator {
	var seed [32]byte
	if _, err := crand.Read(seed[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to
		// the runtime's own random source rather than a fixed seed.
		for i := 0; i < len(seed); i += 8 {
			binary.LittleEndian.PutUint64(seed[i:], rand.Uint64())
		}
	}
	return &idGenerator{rng: rand.NewChaCha8(seed)}
}

func (g *idGenerator) traceID() TraceID {
	g.mu.Lock()
	defer g.mu.Unlock()
	var t TraceID
	for !t.IsValid() {
		binary.LittleEndian.PutUint64(t[:8], g.rng.Uint64())
		binary.LittleEndian.PutUint64(t[8:], g.rng.Uint64())
	}
	return t
}

func (g *idGenerator) spanID() SpanID {
	g.mu.Lock()
	defer g.mu.Unlock()
	var s SpanID
	for !s.IsValid() {
		binary.LittleEndian.PutUint64(s[:], g.rng.Uint64())
	}
	return s
}

// newUUID returns a random RFC 4122 version 4 UUID, used for
// service.instance.id.
func newUUID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		binary.LittleEndian.PutUint64(b[:8], rand.Uint64())
		binary.LittleEndian.PutUint64(b[8:], rand.Uint64())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
