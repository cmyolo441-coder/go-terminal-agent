package collab

// ringEntry is one sequenced event, pre-encoded as an SSE frame.
type ringEntry struct {
	seq   uint64
	frame []byte
}

// ringLog keeps the most recent sequenced events for Last-Event-ID resume.
// Sequence numbers are dense: every event with a Seq goes through append.
type ringLog struct {
	buf   []ringEntry
	start int // index of the oldest entry
	n     int
	last  uint64 // highest sequence appended
}

func newRingLog(capacity int) *ringLog {
	return &ringLog{buf: make([]ringEntry, max(capacity, 1))}
}

func (r *ringLog) append(seq uint64, frame []byte) {
	idx := (r.start + r.n) % len(r.buf)
	if r.n == len(r.buf) {
		r.buf[r.start] = ringEntry{}
		r.start = (r.start + 1) % len(r.buf)
		idx = (r.start + r.n - 1) % len(r.buf)
	} else {
		r.n++
	}
	r.buf[idx] = ringEntry{seq: seq, frame: frame}
	r.last = seq
}

// since returns the frames of every event after seq, or ok=false when some
// of them were already evicted (or seq is from the future).
func (r *ringLog) since(seq uint64) (frames [][]byte, ok bool) {
	if seq > r.last {
		return nil, false
	}
	if seq == r.last {
		return nil, true
	}
	if r.n == 0 {
		return nil, false
	}
	oldest := r.buf[r.start].seq
	if seq+1 < oldest {
		return nil, false
	}
	skip := int(seq + 1 - oldest)
	for i := skip; i < r.n; i++ {
		frames = append(frames, r.buf[(r.start+i)%len(r.buf)].frame)
	}
	return frames, true
}
