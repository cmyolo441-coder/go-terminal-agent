package sentinel

import (
	"fmt"
	"testing"
	"time"
)

// steadyStream is a realistic, non-anomalous mix: distinct reads, commands
// and writes, results, and model calls, at a steady pace.
type steadyStream struct {
	evs []Event
	i   int
	now time.Time
}

func newSteadyStream(n int) *steadyStream {
	st := &steadyStream{now: t0}
	for i := 0; i < n; i++ {
		var ev Event
		switch i % 6 {
		case 0:
			ev = Event{Kind: KindFileRead, Tool: "read", Target: fmt.Sprintf("/work/repo/internal/pkg%d/file_%d.go", i%17, i)}
		case 1:
			ev = Event{Kind: KindExec, Tool: "bash", Target: fmt.Sprintf("go test ./internal/pkg%d/... -run 'TestCase%d' -count=1", i%13, i)}
		case 2:
			ev = Event{Kind: KindToolResult, Tool: "bash", Target: "", Bytes: 512}
		case 3:
			ev = Event{Kind: KindFileWrite, Tool: "edit", Target: fmt.Sprintf("/work/repo/internal/pkg%d/file_%d.go", i%11, i), Bytes: 2048, Digest: uint64(i)*2654435761 + 1}
		case 4:
			ev = Event{Kind: KindLLMRequest, Tool: "claude", Target: ""}
		case 5:
			ev = Event{Kind: KindLLMResponse, Tool: "claude", Tokens: 1500, Cost: 0.002}
		}
		ev.Session = "bench"
		st.evs = append(st.evs, ev)
	}
	return st
}

func (st *steadyStream) next() Event {
	ev := st.evs[st.i%len(st.evs)]
	st.i++
	st.now = st.now.Add(500 * time.Millisecond)
	ev.Time = st.now
	return ev
}

func benchSentinel(tb testing.TB) *Sentinel {
	s, err := New(Config{Clock: NewManualClock(t0), Burn: BurnConfig{TokenBudget: 1 << 40}})
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

func warm(s *Sentinel, st *steadyStream) {
	for i := 0; i < 4096; i++ {
		s.Observe(st.next())
	}
}

func BenchmarkObserve(b *testing.B) {
	s := benchSentinel(b)
	st := newSteadyStream(1024)
	warm(s, st)
	if n := len(s.Incidents()); n != 0 {
		b.Fatalf("steady stream raised %d incidents: %+v", n, s.Incidents()[0])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Observe(st.next())
	}
}

func BenchmarkObserveParallel(b *testing.B) {
	s := benchSentinel(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		st := newSteadyStream(1024)
		sess := fmt.Sprintf("p%p", st)
		for i := range st.evs {
			st.evs[i].Session = sess
		}
		for pb.Next() {
			s.Observe(st.next())
		}
	})
}

func BenchmarkObserveExecLexing(b *testing.B) {
	s := benchSentinel(b)
	st := newSteadyStream(1024)
	warm(s, st)
	// Realistic compound command lines, distinct enough that no detector
	// fires, so this measures lexing and matching alone.
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa"}
	evs := make([]Event, 64)
	for i := range evs {
		x, y, z := words[i%16], words[(i/16+3*i)%16], words[(i*7+5)%16]
		evs[i] = Event{Session: "bench", Kind: KindExec, Tool: "bash",
			Target: fmt.Sprintf(`cd /work/%s && FOO=%s go build -o bin/%s ./cmd/%s 2>&1 | tee %s.log; echo "%s %d"`, x, y, z, x, y, z, i)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev := evs[i%len(evs)]
		st.now = st.now.Add(time.Second)
		ev.Time = st.now
		s.Observe(ev)
	}
	b.StopTimer()
	if n := len(s.Incidents()); n != 0 {
		b.Fatalf("exec stream raised %d incidents, first %+v", n, s.Incidents()[0].Summary)
	}
}

func TestObserveZeroAlloc(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation test")
	}
	s := benchSentinel(t)
	st := newSteadyStream(1024)
	warm(s, st)
	allocs := testing.AllocsPerRun(5000, func() { s.Observe(st.next()) })
	if allocs != 0 {
		t.Fatalf("Observe allocates %.2f times per event on the steady path", allocs)
	}
	if n := len(s.Incidents()); n != 0 {
		t.Fatalf("steady stream raised %d incidents", n)
	}
}
