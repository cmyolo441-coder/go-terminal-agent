package workflow

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeJournal(t *testing.T, n int) (string, []int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, err := CreateJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int64
	for i := range n {
		if err := j.Append(&Record{Type: RecStepSucceeded, Step: "s", Attempt: i + 1, Outputs: map[string]any{"i": float64(i)}}); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(path)
		sizes = append(sizes, fi.Size())
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return path, sizes
}

func TestJournalRoundTrip(t *testing.T) {
	path, _ := writeJournal(t, 3)
	recs, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Seq != 3 || recs[2].Outputs["i"] != 2.0 {
		t.Fatalf("records = %+v", recs)
	}
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if len(line) < 10 || line[8] != ' ' || line[9] != '{' {
			t.Fatalf("line format: %q", line)
		}
	}
	// Appending after reopen continues the sequence.
	j, recs, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(&Record{Type: RecRunResumed}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	recs, _ = ReadJournal(path)
	if len(recs) != 4 || recs[3].Seq != 4 {
		t.Fatalf("after append: %d records", len(recs))
	}
}

func TestJournalTornTailRecovery(t *testing.T) {
	for _, tear := range []int64{1, 5, 30} {
		path, sizes := writeJournal(t, 3)
		if err := os.Truncate(path, sizes[2]-tear); err != nil {
			t.Fatal(err)
		}
		j, recs, err := OpenJournal(path)
		if err != nil {
			t.Fatalf("tear %d: %v", tear, err)
		}
		if len(recs) != 2 || j.TornBytes != sizes[2]-tear-sizes[1] {
			t.Fatalf("tear %d: %d records, %d torn bytes", tear, len(recs), j.TornBytes)
		}
		if err := j.Append(&Record{Type: RecRunResumed}); err != nil {
			t.Fatal(err)
		}
		j.Close()
		recs, err = ReadJournal(path)
		if err != nil || len(recs) != 3 || recs[2].Type != RecRunResumed || recs[2].Seq != 3 {
			t.Fatalf("tear %d after recovery: %v %+v", tear, err, recs)
		}
	}

	// A complete final line with a bad checksum is also a torn tail.
	path, sizes := writeJournal(t, 2)
	data, _ := os.ReadFile(path)
	data[sizes[1]-3] ^= 0xff
	os.WriteFile(path, data, 0o600)
	_, recs, err := OpenJournal(path)
	if err != nil || len(recs) != 1 {
		t.Fatalf("bad checksum tail: %v, %d records", err, len(recs))
	}
}

func TestJournalCorruptionIsNotRepaired(t *testing.T) {
	path, sizes := writeJournal(t, 3)
	data, _ := os.ReadFile(path)
	corrupt := bytes.Clone(data)
	corrupt[sizes[0]+20] ^= 0x01 // inside record 2, with record 3 after it
	os.WriteFile(path, corrupt, 0o600)
	_, _, err := OpenJournal(path)
	var ce *CorruptJournalError
	if !errors.As(err, &ce) || ce.Offset != sizes[0] || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, corrupt) {
		t.Fatal("corrupt journal was modified")
	}

	// Records out of sequence are corruption too.
	path2, sizes2 := writeJournal(t, 3)
	data2, _ := os.ReadFile(path2)
	swapped := append(append(bytes.Clone(data2[sizes2[0]:sizes2[1]]), data2[:sizes2[0]]...), data2[sizes2[1]:]...)
	os.WriteFile(path2, swapped, 0o600)
	if _, err := ReadJournal(path2); !strings.Contains(errString(err), "sequence 2 where 1 was expected") {
		t.Fatalf("err = %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestResumeRejectsChangedPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wf.jsonl")
	env := map[string]string{"OSES": `["linux"]`}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	spec := mustSpec(t, `{"steps": [{"id": "m", "type": "noop", "matrix": {"os": "${fromJSON(env.OSES)}"}}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupt before anything runs
	e := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), func(o *Options) { o.LookupEnv = lookup })
	if _, err := e.Start(ctx, spec, nil, path); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("start: %v", err)
	}
	env["OSES"] = `["linux", "darwin"]`
	if _, err := e.Resume(context.Background(), path); err == nil || !strings.Contains(err.Error(), "no longer expands") {
		t.Fatalf("resume: %v", err)
	}
	env["OSES"] = `["linux"]`
	res, err := e.Resume(context.Background(), path)
	if err != nil || res.Count(StepSucceeded) != 1 {
		t.Fatalf("resume: %v", err)
	}
	if _, ok := res.Step("m[os=linux]"); !ok {
		t.Fatal("instance missing")
	}
}

func TestTimelineRender(t *testing.T) {
	t0 := time.Unix(1000, 0)
	entries := []TimelineEntry{
		{Step: "fetch", Status: StepSucceeded, Start: t0, End: t0.Add(time.Second), Attempts: 1},
		{Step: "build[os=linux]", Status: StepSucceeded, Start: t0.Add(time.Second), End: t0.Add(3 * time.Second), Attempts: 2},
		{Step: "deploy", Status: StepFailed, Start: t0.Add(3 * time.Second), End: t0.Add(4 * time.Second), Attempts: 1, Compensation: CompensationSucceeded},
		{Step: "notify", Status: StepSkipped},
	}
	var b strings.Builder
	if err := RenderTimeline(&b, entries, TimelineOptions{Width: 20, ASCII: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	want := []string{
		"step            |0s       2.00s       4.00s|",
		"fetch           |#####...............|   1.00s  succeeded",
		"build[os=linux] |.....##########.....|   2.00s  succeeded (2 attempts)",
		"deploy          |...............#####|   1.00s  failed, compensation succeeded",
		"notify          |....................|       -  skipped",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// From a real run's journal records.
	path := filepath.Join(t.TempDir(), "wf.jsonl")
	spec := mustSpec(t, `{"steps": [{"id": "a", "type": "noop"}, {"id": "b", "type": "noop", "depends_on": ["a"]}]}`)
	res, err := newEngine(t, NewBuiltinRegistry(BuiltinOptions{}), nil).Start(context.Background(), spec, nil, path)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := ReadJournal(path)
	fromLog := TimelineFromRecords(recs)
	if len(fromLog) != 2 || fromLog[1].Step != "b" || fromLog[1].Status != StepSucceeded {
		t.Fatalf("timeline from records = %+v", fromLog)
	}
	b.Reset()
	if err := RenderTimeline(&b, res.Timeline(), TimelineOptions{}); err != nil || !strings.Contains(b.String(), "█") {
		t.Fatalf("render: %v\n%s", err, b.String())
	}
}
