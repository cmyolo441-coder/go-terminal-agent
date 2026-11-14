package fleet

import (
	"container/heap"
	"container/list"
	"slices"
	"time"
)

// readyQueues holds dispatchable tasks. Dispatch is strict by priority
// band; inside a band, tenants take turns by deficit round robin (Shreedhar
// & Varghese), each turn granting a tenant up to its weight in tasks. A
// tenant with nothing the polling worker can run is passed over without
// losing its remaining deficit, so capability filtering does not distort
// the shares of tenants whose tasks every worker can run.
type readyQueues struct {
	bands   [numPriorities]*band
	where   map[string]*list.Element // task ID -> element in its tenant queue
	weight  func(tenant string) int
	maxScan int
}

type band struct {
	queues  map[string]*list.List // tenant -> tasks ordered by Seq
	ring    []string              // tenants with ready tasks, in turn order
	cursor  int
	deficit map[string]int
}

func newReadyQueues(weight func(string) int, maxScan int) *readyQueues {
	rq := &readyQueues{where: map[string]*list.Element{}, weight: weight, maxScan: maxScan}
	for i := range rq.bands {
		rq.bands[i] = &band{queues: map[string]*list.List{}, deficit: map[string]int{}}
	}
	return rq
}

func (rq *readyQueues) band(p Priority) *band {
	return rq.bands[min(max(int(p), 0), int(numPriorities)-1)]
}

func (rq *readyQueues) len() int { return len(rq.where) }

// push adds t, keeping its tenant queue in submission order so retried
// tasks do not lose their place to newer ones.
func (rq *readyQueues) push(t *Task) {
	if _, ok := rq.where[t.ID]; ok {
		return
	}
	b := rq.band(t.Priority)
	q := b.queues[t.Tenant]
	if q == nil {
		q = list.New()
		b.queues[t.Tenant] = q
		b.ring = append(b.ring, t.Tenant)
	}
	e := q.Back()
	for e != nil && e.Value.(*Task).Seq > t.Seq {
		e = e.Prev()
	}
	if e == nil {
		rq.where[t.ID] = q.PushFront(t)
	} else {
		rq.where[t.ID] = q.InsertAfter(t, e)
	}
}

// remove drops t (which must be the instance pushed, or one with the same
// ID, tenant and priority).
func (rq *readyQueues) remove(t *Task) {
	e, ok := rq.where[t.ID]
	if !ok {
		return
	}
	delete(rq.where, t.ID)
	b := rq.band(t.Priority)
	q := b.queues[t.Tenant]
	q.Remove(e)
	if q.Len() > 0 {
		return
	}
	delete(b.queues, t.Tenant)
	delete(b.deficit, t.Tenant)
	i := slices.Index(b.ring, t.Tenant)
	if i < 0 {
		return
	}
	b.ring = slices.Delete(b.ring, i, i+1)
	if i < b.cursor {
		b.cursor--
	}
	if b.cursor >= len(b.ring) {
		b.cursor = 0
	}
}

// next picks the task to give a worker for which match is true, or nil.
// It does not remove the task; the caller's state transition does.
func (rq *readyQueues) next(match func(*Task) bool) *Task {
	for p := numPriorities - 1; p >= 0; p-- {
		if t := rq.bands[p].pick(match, rq.weight, rq.maxScan); t != nil {
			return t
		}
	}
	return nil
}

func (b *band) pick(match func(*Task) bool, weight func(string) int, maxScan int) *Task {
	n := len(b.ring)
	if n == 0 {
		return nil
	}
	if b.cursor >= n {
		b.cursor = 0
	}
	for range n {
		tenant := b.ring[b.cursor]
		if b.deficit[tenant] <= 0 {
			b.deficit[tenant] = max(weight(tenant), 1)
		}
		if t := scan(b.queues[tenant], match, maxScan); t != nil {
			b.deficit[tenant]--
			if b.deficit[tenant] <= 0 {
				b.cursor = (b.cursor + 1) % n
			}
			return t
		}
		b.cursor = (b.cursor + 1) % n
	}
	return nil
}

func scan(q *list.List, match func(*Task) bool, maxScan int) *Task {
	i := 0
	for e := q.Front(); e != nil && i < maxScan; e = e.Next() {
		t := e.Value.(*Task)
		if match(t) {
			return t
		}
		i++
	}
	return nil
}

// deadlineHeap is a min-heap of (task, time) entries with lazy deletion:
// entries are checked against the task's current state when popped.
type deadlineHeap []deadlineEntry

type deadlineEntry struct {
	id string
	at time.Time
}

func (h deadlineHeap) Len() int           { return len(h) }
func (h deadlineHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h deadlineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *deadlineHeap) Push(x any)        { *h = append(*h, x.(deadlineEntry)) }
func (h *deadlineHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

func (h *deadlineHeap) add(id string, at time.Time) { heap.Push(h, deadlineEntry{id, at}) }

func (h deadlineHeap) peek() (deadlineEntry, bool) {
	if len(h) == 0 {
		return deadlineEntry{}, false
	}
	return h[0], true
}

func (h *deadlineHeap) pop() deadlineEntry { return heap.Pop(h).(deadlineEntry) }

// compact drops entries for which live is false, when stale entries
// dominate; it bounds growth from repeated lease extensions.
func (h *deadlineHeap) compact(liveCount int, live func(deadlineEntry) bool) {
	if len(*h) < 1024 || len(*h) < 4*liveCount {
		return
	}
	kept := (*h)[:0]
	for _, e := range *h {
		if live(e) {
			kept = append(kept, e)
		}
	}
	clear((*h)[len(kept):])
	*h = kept
	heap.Init(h)
}
