package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func lastSegment(t *testing.T, dir string) string {
	t.Helper()
	segs, err := listNumbered(dir, walPrefix, walSuffix)
	if err != nil || len(segs) == 0 {
		t.Fatalf("segments: %v %v", segs, err)
	}
	return filepath.Join(dir, segName(segs[len(segs)-1]))
}

func appendTasks(t *testing.T, w *WAL, token uint64, n int, prefix string) {
	t.Helper()
	for i := range n {
		task := &Task{ID: fmt.Sprintf("%s%d", prefix, i), Tenant: "t", Kind: "k", State: StateReady, Seq: uint64(i + 1)}
		if _, err := w.Append(token, opPut, task, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWALRoundTripAndTornTail(t *testing.T) {
	for name, tail := range map[string]string{
		"partial line": `0badc0de {"seq":12,"tok":1,"op":"put","task":{"id":"zz"`,
		"bad checksum": "00000000 {\"seq\":12,\"tok\":1,\"op\":\"del\",\"id\":\"a1\"}\n",
		"garbage":      "\x00\x00\x00\x00\x00",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			w, rec, err := OpenWAL(dir, WALOptions{SyncWrites: true})
			if err != nil || len(rec.Tasks) != 0 {
				t.Fatalf("open: %v %+v", err, rec)
			}
			appendTasks(t, w, 1, 10, "a")
			if _, err := w.Append(1, opDelete, nil, "a3"); err != nil {
				t.Fatal(err)
			}
			w.Close()
			seg := lastSegment(t, dir)
			before, _ := os.Stat(seg)
			f, _ := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0)
			f.WriteString(tail)
			f.Close()
			w, rec, err = OpenWAL(dir, WALOptions{})
			if err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			defer w.Close()
			if len(rec.Tasks) != 9 || rec.Tasks["a3"] != nil || rec.Tasks["a1"] == nil {
				t.Fatalf("recovered %d tasks", len(rec.Tasks))
			}
			if rec.TruncatedBytes != int64(len(tail)) || rec.LastSeq != 11 {
				t.Fatalf("truncated %d (want %d), last seq %d", rec.TruncatedBytes, len(tail), rec.LastSeq)
			}
			after, _ := os.Stat(seg)
			if after.Size() != before.Size() {
				t.Fatalf("segment size %d, want %d", after.Size(), before.Size())
			}
			// Appending after recovery continues the sequence cleanly.
			seq, err := w.Append(1, opPut, &Task{ID: "new" + name[:3]}, "")
			if err != nil || seq != 12 {
				t.Fatalf("append after recovery: seq %d, %v", seq, err)
			}
			w.Close()
			w2, rec2, err := OpenWAL(dir, WALOptions{})
			if err != nil || rec2.Tasks["new"+name[:3]] == nil {
				t.Fatalf("reopen: %v", err)
			}
			w2.Close()
		})
	}
}

func TestWALMidLogCorruptionIsFatal(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := OpenWAL(dir, WALOptions{})
	appendTasks(t, w, 1, 5, "a")
	w.Close()
	seg := lastSegment(t, dir)
	b, _ := os.ReadFile(seg)
	b[20] ^= 0xff // flip a byte inside the first record
	os.WriteFile(seg, b, 0o600)
	if _, _, err := OpenWAL(dir, WALOptions{}); err == nil {
		t.Fatal("corruption followed by valid records must not be truncated silently")
	} else {
		wantErr(t, err, ErrCorruptLog)
	}
}

func TestWALSnapshotCompactionAndFallback(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := OpenWAL(dir, WALOptions{KeepSnapshots: 2})
	var tasks []*Task
	for round := range 5 {
		for i := range 10 {
			task := &Task{ID: fmt.Sprintf("r%d-%d", round, i), State: StateSucceeded}
			tasks = append(tasks, task)
			w.Append(1, opPut, task, "")
		}
		if err := w.Snapshot(1, tasks, t0); err != nil {
			t.Fatal(err)
		}
	}
	appendTasks(t, w, 1, 3, "tail")
	w.Close()
	snaps, _ := listNumbered(dir, snapPrefix, snapSuffix)
	segs, _ := listNumbered(dir, walPrefix, walSuffix)
	if len(snaps) != 2 || len(segs) > 3 {
		t.Fatalf("after compaction: snapshots %v segments %v", snaps, segs)
	}
	_, rec, err := OpenWAL(dir, WALOptions{})
	if err != nil || len(rec.Tasks) != 53 || rec.SnapshotSeq != 50 {
		t.Fatalf("recover: %v, %d tasks, snapshot %d", err, len(rec.Tasks), rec.SnapshotSeq)
	}
	// A damaged newest snapshot falls back to the previous one plus the
	// segments after it.
	os.WriteFile(filepath.Join(dir, snapName(snaps[1])), []byte("garbage"), 0o600)
	_, rec, err = OpenWAL(dir, WALOptions{})
	if err != nil || len(rec.Tasks) != 53 || rec.SnapshotSeq != 40 {
		t.Fatalf("fallback recover: %v, %d tasks, snapshot %d", err, len(rec.Tasks), rec.SnapshotSeq)
	}
}

func TestWALIgnoresStaleLeaderRecords(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := OpenWAL(dir, WALOptions{})
	w.Append(1, opPut, &Task{ID: "old-leader"}, "")
	w.Append(2, opPut, &Task{ID: "new-leader"}, "")
	// A deposed leader (token 1) slips a write in after the new one.
	w.Append(1, opPut, &Task{ID: "zombie"}, "")
	w.Append(1, opDelete, nil, "new-leader")
	w.Close()
	_, rec, err := OpenWAL(dir, WALOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Tasks["zombie"] != nil || rec.Tasks["new-leader"] == nil || rec.Tasks["old-leader"] == nil {
		t.Fatalf("tasks = %v", rec.Tasks)
	}
	if rec.StaleRecords != 2 || rec.MaxToken != 2 {
		t.Fatalf("stale %d, max token %d", rec.StaleRecords, rec.MaxToken)
	}
}
