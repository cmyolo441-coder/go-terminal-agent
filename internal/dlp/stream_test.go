package dlp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

var streamSample = strings.Join([]string{
	"Customer record follows. card 4111 1111 1111 1111 exp 12/29.",
	"ssn 536-22-1234; mail jane.doe@corp.com; call +44 20 7946 0958.",
	"IBAN: DE89 3704 0044 0532 0130 00 and Aadhaar 2345 6789 0124.",
	"PAN ABCPE1234F, NINO AB 12 34 56 C, client ip 8.8.8.8, DOB: 12/05/1985.",
	td3L1, td3L2,
	"Diagnosis: E11.9, I10. MRN: 00123456. Done.",
}, "\n")

func fixedVault(t testing.TB) *Vault {
	v, err := NewVault([]byte("stream-test-key-0123456789abcdef"), 0)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestWriterBoundaries splits the input at every byte position, with tiny
// internal chunks, and requires output identical to Process.
func TestWriterBoundaries(t *testing.T) {
	e := mustEngine(t, nil)
	want, err := e.Process(streamSample, fixedVault(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 7, 64, 1 << 15} {
		for split := 0; split <= len(streamSample); split += 1 + split/40 {
			var out bytes.Buffer
			var seen []Finding
			w := e.NewWriter(&out, StreamOptions{Vault: fixedVault(t), ChunkSize: chunk, OnFinding: func(f Finding) { seen = append(seen, f) }})
			for _, part := range []string{streamSample[:split], streamSample[split:]} {
				if n, err := w.Write([]byte(part)); err != nil || n != len(part) {
					t.Fatalf("write: %d %v", n, err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if out.String() != want.Text {
				t.Fatalf("chunk %d split %d:\n got %q\nwant %q", chunk, split, out.String(), want.Text)
			}
			if len(seen) != len(want.Findings) {
				t.Fatalf("chunk %d split %d: %d findings, want %d", chunk, split, len(seen), len(want.Findings))
			}
			for i := range seen {
				if seen[i].Start != want.Findings[i].Start || seen[i].Type != want.Findings[i].Type {
					t.Fatalf("finding %d: %v vs %v", i, seen[i], want.Findings[i])
				}
			}
			r := w.Report()
			if r.Findings != want.Report.Findings || r.Bytes != int64(len(streamSample)) || r.Label != Restricted {
				t.Fatalf("report %+v", r)
			}
		}
	}
}

func TestWriterByteAtATime(t *testing.T) {
	e := mustEngine(t, nil)
	want, _ := e.Process(streamSample, fixedVault(t))
	var out bytes.Buffer
	w := e.NewWriter(&out, StreamOptions{Vault: fixedVault(t), ChunkSize: 3})
	for i := 0; i < len(streamSample); i++ {
		w.Write([]byte{streamSample[i]})
	}
	w.Close()
	if out.String() != want.Text {
		t.Fatalf("got %q", out.String())
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestReaderBoundaries(t *testing.T) {
	e := mustEngine(t, nil)
	want, _ := e.Process(streamSample, fixedVault(t))
	for _, chunk := range []int{1, 13, 1 << 15} {
		r := e.NewReader(iotest.OneByteReader(strings.NewReader(streamSample)), StreamOptions{Vault: fixedVault(t), ChunkSize: chunk})
		got, err := io.ReadAll(iotest.HalfReader(r))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want.Text {
			t.Fatalf("chunk %d: got %q", chunk, got)
		}
		if r.Report().Findings != len(want.Findings) {
			t.Fatalf("report %+v", r.Report())
		}
	}
	if err := iotest.TestReader(e.NewReader(strings.NewReader(streamSample), StreamOptions{Vault: fixedVault(t)}), []byte(want.Text)); err != nil {
		t.Fatal(err)
	}
}

func TestStreamLargeBoundedMemory(t *testing.T) {
	e := mustEngine(t, nil)
	unit := "lorem ipsum dolor sit amet, card 4111 1111 1111 1111 and mail a.b@corp.com ok\n"
	src := strings.Repeat(unit, 20000) // ~1.6 MB
	var out bytes.Buffer
	w := e.NewWriter(&out, StreamOptions{ChunkSize: 4096})
	maxBuf := 0
	for i := 0; i < len(src); i += 1000 {
		w.Write([]byte(src[i:min(len(src), i+1000)]))
		maxBuf = max(maxBuf, cap(w.c.buf))
	}
	w.Close()
	if limit := 4 * (4096 + w.c.carry + w.c.look); maxBuf > limit {
		t.Fatalf("buffer grew to %d (limit %d)", maxBuf, limit)
	}
	if strings.Contains(out.String(), "4111") || strings.Count(out.String(), "[REDACTED:CREDIT_CARD]") != 20000 {
		t.Fatal("stream missed values")
	}
	if w.Report().ByType[Email] != 20000 {
		t.Fatalf("report %+v", w.Report())
	}
}

func TestStreamBlock(t *testing.T) {
	e := mustEngine(t, policyJSON(t, `{"version":1,"detectors":{"US_SSN":{"action":"block"}}}`))
	var out bytes.Buffer
	w := e.NewWriter(&out, StreamOptions{ChunkSize: 16})
	head := strings.Repeat("safe text here. ", 200)
	_, err := w.Write([]byte(head + "ssn 536-22-1234 " + strings.Repeat("tail ", 400)))
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(out.String(), "536-22") || !strings.HasPrefix(head, out.String()) {
		t.Fatalf("leaked or corrupted output: %q", out.String())
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, ErrBlocked) {
		t.Fatal("writes continue after block")
	}
	if w.Report().Blocked != 1 {
		t.Fatalf("report %+v", w.Report())
	}
	r := e.NewReader(strings.NewReader("ok then ssn 536-22-1234"), StreamOptions{})
	if _, err := io.ReadAll(r); !errors.Is(err, ErrBlocked) {
		t.Fatalf("reader err %v", err)
	}
}

func TestDetokenizeWriter(t *testing.T) {
	v := fixedVault(t)
	e := mustEngine(t, policyJSON(t, `{"version":1,"default_action":"tokenize"}`))
	res, _ := e.Process(streamSample, v)
	model := "Echo: " + res.Text + " [[ not a token ]] [[PII:EMAIL:ffffffffffff]] end ["
	want, n := v.Detokenize(model)
	if n == 0 || want != "Echo: "+streamSample+" [[ not a token ]] [[PII:EMAIL:ffffffffffff]] end [" {
		t.Fatalf("detokenize %d %q", n, want)
	}
	for step := 1; step <= 37; step += 3 {
		var out bytes.Buffer
		d := NewDetokenizeWriter(&out, v)
		for i := 0; i < len(model); i += step {
			if _, err := d.Write([]byte(model[i:min(len(model), i+step)])); err != nil {
				t.Fatal(err)
			}
		}
		d.Close()
		if out.String() != want || d.Restored() != n {
			t.Fatalf("step %d: got %q (%d)", step, out.String(), d.Restored())
		}
	}
}

// FuzzStream checks that the streaming writer agrees with Process for any
// input and any split.
func FuzzStream(f *testing.F) {
	f.Add(streamSample, 10, 3)
	f.Add("ssn 536-22-1234 mail a@corp.com", 5, 1)
	f.Add("4111 1111 1111 1111", 9, 2)
	e := mustEngine(f, nil)
	f.Fuzz(func(t *testing.T, s string, split, chunk int) {
		if len(s) > 4096 {
			s = s[:4096]
		}
		if split < 0 {
			split = -split
		}
		split %= len(s) + 1
		if chunk < 1 {
			chunk = 1
		}
		chunk = chunk%512 + 1
		want, _ := e.Process(s, fixedVault(t))
		for _, f := range want.Findings {
			if f.Start < 0 || f.End > len(s) || f.Start >= f.End {
				t.Fatalf("bad span %v", f)
			}
		}
		var out bytes.Buffer
		w := e.NewWriter(&out, StreamOptions{Vault: fixedVault(t), ChunkSize: chunk})
		w.Write([]byte(s[:split]))
		w.Write([]byte(s[split:]))
		w.Close()
		if !want.Blocked && out.String() != want.Text {
			t.Fatalf("stream %q != batch %q", out.String(), want.Text)
		}
	})
}
