package memory

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"time"
)

// Frames are the unit of both the segment and the write-ahead log:
//
//	type uint8 | length uint32 LE | crc32c uint32 LE | payload[length]
//
// The CRC-32C (Castagnoli) covers the type, the length and the payload, so
// a torn or bit-flipped frame, or one whose length field was damaged, is
// always detected.
const (
	frameHeaderSize = 9
	maxFramePayload = 1 << 30
)

// Frame types.
const (
	frameRecord byte = 1 // segment: one record (with vector)
	frameGraph  byte = 2 // segment: the HNSW graph
	frameEnd    byte = 3 // segment: trailer with the record count

	walPut    byte = 16 // log: insert or replace a record (with vector)
	walDelete byte = 17 // log: delete record IDs
	walTouch  byte = 18 // log: access times and counts
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func frameCRC(typ byte, payload []byte) uint32 {
	var hdr [5]byte
	hdr[0] = typ
	binary.LittleEndian.PutUint32(hdr[1:], uint32(len(payload)))
	c := crc32.Update(0, castagnoli, hdr[:])
	return crc32.Update(c, castagnoli, payload)
}

// appendFrame appends a framed payload to buf.
func appendFrame(buf []byte, typ byte, payload []byte) []byte {
	buf = append(buf, typ)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(payload)))
	buf = binary.LittleEndian.AppendUint32(buf, frameCRC(typ, payload))
	return append(buf, payload...)
}

// readFrame decodes the frame at the start of data. It returns the frame
// size, or ok=false when data holds no complete, valid frame.
func readFrame(data []byte) (typ byte, payload []byte, size int, ok bool) {
	if len(data) < frameHeaderSize {
		return 0, nil, 0, false
	}
	typ = data[0]
	n := binary.LittleEndian.Uint32(data[1:5])
	if n > maxFramePayload || uint64(n) > uint64(len(data)-frameHeaderSize) {
		return 0, nil, 0, false
	}
	payload = data[frameHeaderSize : frameHeaderSize+int(n)]
	if binary.LittleEndian.Uint32(data[5:9]) != frameCRC(typ, payload) {
		return 0, nil, 0, false
	}
	return typ, payload, frameHeaderSize + int(n), true
}

// storedRecord is a record with its index key and unit vector.
type storedRecord struct {
	Key uint64
	Rec Record
	Vec []float32
}

func appendString(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

func appendTime(buf []byte, t time.Time) []byte {
	if t.IsZero() {
		return binary.AppendVarint(buf, math.MinInt64)
	}
	return binary.AppendVarint(buf, t.UnixNano())
}

func readTime(r *byteReader) time.Time {
	v := r.varint()
	if v == math.MinInt64 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

const recordCodecVersion = 1

// encodeRecord appends the binary form of sr to buf.
func encodeRecord(buf []byte, sr *storedRecord) []byte {
	r := &sr.Rec
	buf = binary.AppendUvarint(buf, recordCodecVersion)
	buf = binary.AppendUvarint(buf, sr.Key)
	buf = appendString(buf, r.ID)
	buf = append(buf, kindCode(r.Kind), scopeCode(r.Scope))
	buf = appendString(buf, r.Text)
	buf = binary.AppendUvarint(buf, uint64(len(r.Tags)))
	for _, t := range r.Tags {
		buf = appendString(buf, t)
	}
	buf = appendString(buf, r.Namespace)
	buf = appendTime(buf, r.Created)
	buf = appendTime(buf, r.Updated)
	buf = appendTime(buf, r.LastAccess)
	buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(r.Importance))
	buf = binary.AppendVarint(buf, int64(r.TTL))
	buf = appendString(buf, r.Source)
	buf = binary.AppendUvarint(buf, uint64(r.AccessCount))
	buf = binary.AppendUvarint(buf, uint64(r.Reinforced))
	buf = binary.AppendUvarint(buf, uint64(len(r.MergedFrom)))
	for _, id := range r.MergedFrom {
		buf = appendString(buf, id)
	}
	buf = binary.AppendUvarint(buf, uint64(len(sr.Vec)))
	return appendVector(buf, sr.Vec)
}

// decodeRecord parses a record payload and validates it.
func decodeRecord(data []byte) (*storedRecord, error) {
	r := byteReader{b: data}
	if v := r.uvarint(); v != recordCodecVersion {
		if r.err != nil {
			return nil, r.err
		}
		return nil, fmt.Errorf("%w: record codec version %d", ErrCorrupt, v)
	}
	sr := &storedRecord{Key: r.uvarint()}
	rec := &sr.Rec
	rec.ID = r.string()
	kc, sc := r.byte(), r.byte()
	rec.Text = r.string()
	ntags := r.uvarint()
	if ntags > maxTags {
		return nil, fmt.Errorf("%w: %d tags", ErrCorrupt, ntags)
	}
	for i := uint64(0); i < ntags && r.err == nil; i++ {
		rec.Tags = append(rec.Tags, r.string())
	}
	rec.Namespace = r.string()
	rec.Created = readTime(&r)
	rec.Updated = readTime(&r)
	rec.LastAccess = readTime(&r)
	if raw := r.bytes(8); raw != nil {
		rec.Importance = math.Float64frombits(binary.LittleEndian.Uint64(raw))
	}
	rec.TTL = time.Duration(r.varint())
	rec.Source = r.string()
	rec.AccessCount = uint32(min(r.uvarint(), math.MaxUint32))
	rec.Reinforced = uint32(min(r.uvarint(), math.MaxUint32))
	nm := r.uvarint()
	if nm > maxMergedFrom {
		return nil, fmt.Errorf("%w: %d merged ids", ErrCorrupt, nm)
	}
	for i := uint64(0); i < nm && r.err == nil; i++ {
		rec.MergedFrom = append(rec.MergedFrom, r.string())
	}
	dim := r.uvarint()
	if dim > 1<<16 {
		return nil, fmt.Errorf("%w: vector dimension %d", ErrCorrupt, dim)
	}
	if raw := r.bytes(4 * int(dim)); r.err == nil && dim > 0 {
		sr.Vec = decodeVector(raw, int(dim))
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.off != len(data) {
		return nil, fmt.Errorf("%w: trailing bytes in record", ErrCorrupt)
	}
	var ok bool
	if rec.Kind, ok = kindFromCode(kc); !ok {
		return nil, fmt.Errorf("%w: kind code %d", ErrCorrupt, kc)
	}
	if rec.Scope, ok = scopeFromCode(sc); !ok {
		return nil, fmt.Errorf("%w: scope code %d", ErrCorrupt, sc)
	}
	if rec.ID == "" || rec.Text == "" || !(rec.Importance > 0 && rec.Importance <= 1) || rec.TTL < 0 {
		return nil, fmt.Errorf("%w: invalid record %q", ErrCorrupt, rec.ID)
	}
	return sr, nil
}

// encodeIDs is the payload of a delete frame.
func encodeIDs(ids []string) []byte {
	buf := binary.AppendUvarint(nil, uint64(len(ids)))
	for _, id := range ids {
		buf = appendString(buf, id)
	}
	return buf
}

func decodeIDs(data []byte) ([]string, error) {
	r := byteReader{b: data}
	n := r.uvarint()
	if n > uint64(len(data)) {
		return nil, fmt.Errorf("%w: id count", ErrCorrupt)
	}
	ids := make([]string, 0, n)
	for i := uint64(0); i < n && r.err == nil; i++ {
		ids = append(ids, r.string())
	}
	if r.err == nil && r.off != len(data) {
		return nil, fmt.Errorf("%w: trailing bytes in id list", ErrCorrupt)
	}
	return ids, r.err
}

// touch is an access-time update.
type touch struct {
	ID    string
	At    time.Time
	Count uint32 // accesses to add
}

func encodeTouches(ts []touch) []byte {
	buf := binary.AppendUvarint(nil, uint64(len(ts)))
	for _, t := range ts {
		buf = appendString(buf, t.ID)
		buf = appendTime(buf, t.At)
		buf = binary.AppendUvarint(buf, uint64(t.Count))
	}
	return buf
}

func decodeTouches(data []byte) ([]touch, error) {
	r := byteReader{b: data}
	n := r.uvarint()
	if n > uint64(len(data)) {
		return nil, fmt.Errorf("%w: touch count", ErrCorrupt)
	}
	ts := make([]touch, 0, n)
	for i := uint64(0); i < n && r.err == nil; i++ {
		ts = append(ts, touch{ID: r.string(), At: readTime(&r), Count: uint32(min(r.uvarint(), math.MaxUint32))})
	}
	if r.err == nil && r.off != len(data) {
		return nil, fmt.Errorf("%w: trailing bytes in touch list", ErrCorrupt)
	}
	return ts, r.err
}
