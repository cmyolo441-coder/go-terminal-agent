package browserview

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLatestSlowConsumerSeesNewest(t *testing.T) {
	l := NewLatest[int]()
	for i := 1; i <= 100; i++ {
		l.Put(i)
	}
	v, ok := l.Take()
	if !ok || v != 100 {
		t.Fatalf("Take = %d,%v want 100", v, ok)
	}
	if _, ok := l.Take(); ok {
		t.Fatal("value must be consumed")
	}
	if l.Dropped() != 99 {
		t.Fatalf("dropped = %d", l.Dropped())
	}
}

func TestLatestConcurrentProducerConsumer(t *testing.T) {
	l := NewLatest[int]()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const n = 5000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			l.Put(i)
		}
	}()
	last := 0
	for last != n {
		v, ok := l.Wait(ctx)
		if !ok {
			t.Fatal("timed out")
		}
		if v <= last {
			t.Fatalf("went backwards: %d after %d", v, last)
		}
		last = v
		time.Sleep(10 * time.Microsecond) // slow consumer
	}
	wg.Wait()
}

func TestPumpCoalescesFrames(t *testing.T) {
	ch := make(chan Frame)
	l := NewLatest[Frame]()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pump(ctx, ch, l, done)
	for i := uint64(1); i <= 50; i++ {
		ch <- Frame{Seq: i} // never blocks for long: pump only Puts
	}
	close(ch)
	<-done
	f, ok := l.Take()
	if !ok || f.Seq != 50 {
		t.Fatalf("newest = %d", f.Seq)
	}
}

func TestPacer(t *testing.T) {
	now := time.Unix(100, 0)
	p := NewPacer(10)
	p.now = func() time.Time { return now }
	if p.Delay() != 0 {
		t.Fatal("first event must not wait")
	}
	p.Mark()
	now = now.Add(30 * time.Millisecond)
	if d := p.Delay(); d != 70*time.Millisecond {
		t.Fatalf("delay = %v", d)
	}
	now = now.Add(time.Second)
	if p.Delay() != 0 {
		t.Fatal("no delay after interval")
	}
	if NewPacer(0).Delay() != 0 {
		t.Fatal("fps<=0 disables pacing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	real := NewPacer(1)
	real.Mark()
	if real.Wait(ctx) {
		t.Fatal("Wait must report cancellation")
	}
}

func TestFPSMeter(t *testing.T) {
	var m fpsMeter
	t0 := time.Unix(0, 0)
	for i := 0; i < 30; i++ {
		m.tick(t0.Add(time.Duration(i) * 50 * time.Millisecond))
	}
	if v := m.value(t0.Add(29 * 50 * time.Millisecond)); v < 19 || v > 21 {
		t.Fatalf("fps = %.1f, want ~20", v)
	}
	if v := m.value(t0.Add(10 * time.Second)); v != 0 {
		t.Fatalf("idle fps = %.1f", v)
	}
}
