package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func baseRequest(prompt string) llm.Request {
	return llm.Request{
		Model:     "claude-sonnet-5",
		System:    "You are a coding agent.",
		Messages:  []llm.Message{llm.UserText(prompt)},
		Tools:     []llm.ToolSpec{{Name: "read_file", Description: "Read a file", Properties: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}}},
		MaxTokens: 4096,
	}
}

func TestCassetteRoundTripStreamedChunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "c.jsonl")
	live := &scriptProvider{name: "anthropic", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		if strings.Contains(req.Messages[0].Text(), "tool") {
			evs, resp := toolReply("toolu_ABC", "read_file", map[string]any{"path": "main.go"})
			return evs, resp, nil
		}
		evs := []llm.Event{
			{Type: llm.EventThinkingDelta, Text: "hmm "},
			{Type: llm.EventThinkingDelta, Text: "ok"},
			{Type: llm.EventBlockStop},
		}
		tevs, resp := textReply("Hel", "lo, ", "wor", "ld")
		resp.Message.Native = json.RawMessage(`{"signature":"abc"}`)
		resp.Message.Provider = "anthropic"
		resp.StopDetail = ""
		return append(evs, tevs...), resp, nil
	}}

	rec, err := OpenRecorder(path, ModeRecord, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var liveLog, liveLog2 eventLog
	resp1, err := rec.Stream(context.Background(), baseRequest("hello"), liveLog.add)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := rec.Stream(context.Background(), baseRequest("use a tool"), liveLog2.add)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if got := live.calls.Load(); got != 2 {
		t.Fatalf("live calls = %d, want 2", got)
	}

	// Replay without any real provider.
	rep, err := OpenRecorder(path, ModeReplay, nil, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	if rep.Name() != "anthropic" {
		t.Errorf("replay name = %q, want recorded provider name", rep.Name())
	}
	var replayLog, replayLog2 eventLog
	got1, err := rep.Stream(context.Background(), baseRequest("hello"), replayLog.add)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := rep.Stream(context.Background(), baseRequest("use a tool"), replayLog2.add)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(liveLog.all(), replayLog.all()) {
		t.Errorf("text events differ:\nlive   %+v\nreplay %+v", liveLog.all(), replayLog.all())
	}
	if !reflect.DeepEqual(liveLog2.all(), replayLog2.all()) {
		t.Errorf("tool events differ:\nlive   %+v\nreplay %+v", liveLog2.all(), replayLog2.all())
	}
	if len(replayLog.all()) != 8 {
		t.Errorf("replayed %d events, want 8 (thinking, text chunks, stops)", len(replayLog.all()))
	}
	if !reflect.DeepEqual(resp1, got1) {
		t.Errorf("response 1 differs:\nlive   %+v\nreplay %+v", resp1, got1)
	}
	if !reflect.DeepEqual(resp2, got2) {
		t.Errorf("response 2 differs:\nlive   %+v\nreplay %+v", resp2, got2)
	}
	// Mutating a replayed response must not corrupt the cassette.
	got1.Message.Blocks[0].Text = "mutated"
	again, err := rep.Session().Stream(context.Background(), baseRequest("hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Message.Text() != "Hello, world" {
		t.Errorf("replayed text after mutation = %q", again.Message.Text())
	}
}

func TestCassetteFileIsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	live := &scriptProvider{name: "p", reply: func(llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := textReply("a<b>&c")
		return evs, resp, nil
	}}
	rec, err := OpenRecorder(path, ModeRecord, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two", "three"} {
		if _, err := rec.Stream(context.Background(), baseRequest(p), nil); err != nil {
			t.Fatal(err)
		}
	}
	rec.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want header + 3", len(lines))
	}
	for i, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %d is not JSON: %v", i+1, err)
		}
	}
	if !strings.Contains(lines[1], "a<b>&c") {
		t.Errorf("HTML characters should not be escaped: %s", lines[1])
	}
	c, err := LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 3 {
		t.Fatalf("loaded %d interactions", c.Len())
	}
	var buf bytes.Buffer
	if _, err := c.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(data) {
		t.Errorf("re-encoding changed the file:\n%s\nvs\n%s", buf.String(), data)
	}
}

func TestRecordMissingAndPassthrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	live := &scriptProvider{name: "p", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := textReply("echo: " + req.Messages[0].Text())
		return evs, resp, nil
	}}
	rec, err := OpenRecorder(path, ModeRecordMissing, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rec.Stream(ctx, baseRequest("a"), nil); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	rec, err = OpenRecorder(path, ModeRecordMissing, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := rec.Session().Stream(ctx, baseRequest("a"), nil)
	if err != nil || r.Message.Text() != "echo: a" {
		t.Fatalf("replayed %v %v", r, err)
	}
	if live.calls.Load() != 1 {
		t.Fatalf("existing request should replay, calls = %d", live.calls.Load())
	}
	if _, err := rec.Stream(ctx, baseRequest("b"), nil); err != nil {
		t.Fatal(err)
	}
	if live.calls.Load() != 2 {
		t.Fatalf("missing request should be recorded, calls = %d", live.calls.Load())
	}
	rec.Close()
	c, err := LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Fatalf("cassette has %d interactions, want 2", c.Len())
	}

	pt, err := OpenRecorder(filepath.Join(t.TempDir(), "never.jsonl"), ModePassthrough, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pt.Stream(ctx, baseRequest("a"), nil); err != nil {
		t.Fatal(err)
	}
	if live.calls.Load() != 3 || pt.Cassette().Len() != 0 {
		t.Fatalf("passthrough should call live and record nothing")
	}
	if _, err := OpenRecorder(filepath.Join(t.TempDir(), "missing.jsonl"), ModeReplay, nil, RecorderOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replay of missing cassette: %v", err)
	}
	if _, err := NewRecorder(ModeRecord, nil, nil, RecorderOptions{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("record without provider: %v", err)
	}
}

func TestReplayMismatchDiff(t *testing.T) {
	live := &scriptProvider{name: "p", reply: func(llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := textReply("ok")
		return evs, resp, nil
	}}
	rec, err := NewRecorder(ModeRecord, nil, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range []string{"fix the bug in parser.go", "something entirely different and unrelated"} {
		if _, err := rec.Stream(ctx, baseRequest(p), nil); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := NewRecorder(ModeReplay, rec.Cassette(), nil, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	req := baseRequest("fix the bug in lexer.go")
	req.MaxTokens = 8192
	_, err = rep.Stream(ctx, req, nil)
	var mm *MismatchError
	if !errors.As(err, &mm) || !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want MismatchError, got %v", err)
	}
	if mm.ClosestSeq != 0 || mm.Recorded != 2 {
		t.Fatalf("closest = %d of %d, want 0 of 2", mm.ClosestSeq, mm.Recorded)
	}
	want := map[string]bool{"max_tokens": false, "messages.0.blocks.0.text": false}
	for _, d := range mm.Diffs {
		if _, ok := want[d.Path]; ok {
			want[d.Path] = true
		}
		if d.Kind != DiffChanged {
			t.Errorf("unexpected diff kind %v", d)
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("diff misses %s: %v", p, mm.Diffs)
		}
	}
	if len(mm.Diffs) != 2 {
		t.Errorf("want exactly 2 diffs, got %v", mm.Diffs)
	}
	msg := err.Error()
	for _, s := range []string{"closest is #0", "~ max_tokens: 4096 -> 8192", `"fix the bug in parser.go" -> "fix the bug in lexer.go"`} {
		if !strings.Contains(msg, s) {
			t.Errorf("error message misses %q:\n%s", s, msg)
		}
	}

	// Structural additions/removals are reported as such.
	req2 := baseRequest("fix the bug in parser.go")
	req2.Messages = append(req2.Messages, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "sure"}}})
	req2.Tools = nil
	_, err = rep.Stream(ctx, req2, nil)
	if !errors.As(err, &mm) {
		t.Fatal(err)
	}
	var added, removed bool
	for _, d := range mm.Diffs {
		if d.Kind == DiffAdded && d.Path == "messages.1" {
			added = true
		}
		if d.Kind == DiffRemoved && d.Path == "tools.0" {
			removed = true
		}
	}
	if !added || !removed {
		t.Errorf("want messages.1 added and tools.0 removed, got %v", mm.Diffs)
	}

	empty, _ := NewRecorder(ModeReplay, NewCassette("x"), nil, RecorderOptions{})
	_, err = empty.Stream(ctx, req, nil)
	if !errors.As(err, &mm) || mm.ClosestSeq != -1 {
		t.Fatalf("empty cassette: %v", err)
	}
}

func TestStringDeltaLongStrings(t *testing.T) {
	a := strings.Repeat("x", 500) + "AAA" + strings.Repeat("y", 500)
	b := strings.Repeat("x", 500) + "BBB" + strings.Repeat("y", 500)
	d := structuralDiff(map[string]any{"s": a}, map[string]any{"s": b})
	if len(d) != 1 {
		t.Fatal(d)
	}
	if !strings.Contains(d[0].Recorded, "AAA") || !strings.Contains(d[0].Actual, "BBB") || !strings.Contains(d[0].Actual, "differs at 500") {
		t.Errorf("excerpt does not centre on the change: %+v", d[0])
	}
	if len(d[0].Recorded) > 200 {
		t.Errorf("excerpt too long: %d", len(d[0].Recorded))
	}
}

func TestScrubbingAndPlaceholders(t *testing.T) {
	const secret = "sk-live-SECRET123"
	scrub := func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }
	ws1 := "/tmp/ws-run-1"
	live := &scriptProvider{name: "p", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := toolReply("toolu_1", "read_file", map[string]any{"path": ws1 + "/main.go", "token": secret})
		resp.Message.Blocks[0].Text = "Reading " + ws1 + "/main.go with " + secret
		return evs, resp, nil
	}}
	rec, err := NewRecorder(ModeRecord, nil, live, RecorderOptions{Scrub: scrub})
	if err != nil {
		t.Fatal(err)
	}
	req := baseRequest("my key is " + secret + ", work in " + ws1)
	ctx := context.Background()
	live1, err := rec.Session(Replacement{Actual: ws1, Placeholder: WorkspacePlaceholder}).Stream(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live1.Message.Blocks[0].Text, secret) {
		t.Error("the live caller must see the unscrubbed response")
	}
	var buf bytes.Buffer
	if _, err := rec.Cassette().WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret leaked into cassette:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), ws1) {
		t.Fatalf("workspace path leaked into cassette:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "$WORKSPACE/main.go") {
		t.Fatalf("placeholder missing:\n%s", buf.String())
	}

	// Replay in a different workspace: the request matches and the
	// response is rewritten to the new path.
	ws2 := "/var/tmp/other-ws"
	rep, err := NewRecorder(ModeReplay, rec.Cassette(), nil, RecorderOptions{Scrub: scrub})
	if err != nil {
		t.Fatal(err)
	}
	req2 := baseRequest("my key is " + secret + ", work in " + ws2)
	got, err := rep.Session(Replacement{Actual: ws2, Placeholder: WorkspacePlaceholder}).Stream(ctx, req2, nil)
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]string
	if err := json.Unmarshal(got.Message.Blocks[1].Input, &in); err != nil {
		t.Fatal(err)
	}
	if in["path"] != ws2+"/main.go" || in["token"] != "[REDACTED]" {
		t.Errorf("replayed input = %v", in)
	}
}

func TestReplayCursorAndStrict(t *testing.T) {
	n := 0
	live := &scriptProvider{name: "p", reply: func(llm.Request) ([]llm.Event, *llm.Response, error) {
		n++
		evs, resp := textReply(strings.Repeat("!", n))
		return evs, resp, nil
	}}
	rec, _ := NewRecorder(ModeRecord, nil, live, RecorderOptions{})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := rec.Session().Stream(ctx, baseRequest("same"), nil); err != nil {
			t.Fatal(err)
		}
	}
	rep, _ := NewRecorder(ModeReplay, rec.Cassette(), nil, RecorderOptions{})
	s := rep.Session()
	var got []string
	for i := 0; i < 3; i++ {
		r, err := s.Stream(ctx, baseRequest("same"), nil)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Message.Text())
	}
	if strings.Join(got, ",") != "!,!!,!!" {
		t.Errorf("replay order = %v, want recordings in order then the last again", got)
	}
	// A fresh session starts from the first recording again.
	r, _ := rep.Session().Stream(ctx, baseRequest("same"), nil)
	if r.Message.Text() != "!" {
		t.Errorf("new session got %q", r.Message.Text())
	}
	strict, _ := NewRecorder(ModeReplay, rec.Cassette(), nil, RecorderOptions{StrictReplay: true})
	ss := strict.Session()
	ss.Stream(ctx, baseRequest("same"), nil)
	ss.Stream(ctx, baseRequest("same"), nil)
	if _, err := ss.Stream(ctx, baseRequest("same"), nil); !errors.Is(err, ErrNoMatch) {
		t.Errorf("strict replay should fail once exhausted, got %v", err)
	}
}

func TestRecordErrorsAndCancellation(t *testing.T) {
	boom := errors.New("overloaded_error: 529")
	live := &scriptProvider{name: "p", reply: func(llm.Request) ([]llm.Event, *llm.Response, error) {
		return []llm.Event{{Type: llm.EventTextDelta, Text: "par"}}, nil, boom
	}}
	ctx := context.Background()
	off, _ := NewRecorder(ModeRecord, nil, live, RecorderOptions{})
	if _, err := off.Stream(ctx, baseRequest("x"), nil); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if off.Cassette().Len() != 0 {
		t.Fatal("errors must not be recorded by default")
	}
	on, _ := NewRecorder(ModeRecord, nil, live, RecorderOptions{RecordErrors: true})
	on.Stream(ctx, baseRequest("x"), nil)
	rep, _ := NewRecorder(ModeReplay, on.Cassette(), nil, RecorderOptions{})
	var log eventLog
	_, err := rep.Stream(ctx, baseRequest("x"), log.add)
	var re *ReplayedError
	if !errors.As(err, &re) || re.Message != boom.Error() {
		t.Fatalf("replayed error = %v", err)
	}
	if len(log.all()) != 1 {
		t.Errorf("partial events before the error should replay too")
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := rep.Stream(cctx, baseRequest("x"), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled replay: %v", err)
	}
}

func TestConcurrentSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	live := &scriptProvider{name: "p", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := textReply("re: ", req.Messages[0].Text())
		return evs, resp, nil
	}}
	rec, err := OpenRecorder(path, ModeRecord, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := rec.Session()
			if _, err := s.Stream(context.Background(), baseRequest(strings.Repeat("q", i+1)), nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	rec.Close()
	c, err := LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 32 {
		t.Fatalf("got %d interactions", c.Len())
	}
	rep, _ := NewRecorder(ModeReplay, c, nil, RecorderOptions{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := strings.Repeat("q", i+1)
			r, err := rep.Session().Stream(context.Background(), baseRequest(p), nil)
			if err != nil || r.Message.Text() != "re: "+p {
				t.Errorf("replay %d: %v %v", i, r, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestParseCassetteSpec(t *testing.T) {
	cases := []struct {
		in   string
		mode Mode
		path string
		bad  bool
	}{
		{in: "replay:a/b.jsonl", mode: ModeReplay, path: "a/b.jsonl"},
		{in: "record:x.jsonl", mode: ModeRecord, path: "x.jsonl"},
		{in: "record-missing:x.jsonl", mode: ModeRecordMissing, path: "x.jsonl"},
		{in: "record_missing:x.jsonl", mode: ModeRecordMissing, path: "x.jsonl"},
		{in: "passthrough", mode: ModePassthrough},
		{in: "off", mode: ModePassthrough},
		{in: "testdata/c.jsonl", mode: ModeReplay, path: "testdata/c.jsonl"},
		{in: `C:\cassettes\c.jsonl`, mode: ModeReplay, path: `C:\cassettes\c.jsonl`},
		{in: "record:", bad: true},
		{in: "  ", bad: true},
	}
	for _, c := range cases {
		m, p, err := ParseCassetteSpec(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want error", c.in)
			}
			continue
		}
		if err != nil || m != c.mode || p != c.path {
			t.Errorf("%q: got %q %q %v", c.in, m, p, err)
		}
	}
	if _, err := ParseMode("bogus"); err == nil {
		t.Error("bogus mode accepted")
	}

	live := &scriptProvider{name: "p", reply: func(llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := textReply("hi")
		return evs, resp, nil
	}}
	p, closer, err := WrapProvider("", live, RecorderOptions{})
	if err != nil || p != llm.Provider(live) {
		t.Fatal("empty spec should return the provider unchanged")
	}
	closer.Close()
	path := filepath.Join(t.TempDir(), "w.jsonl")
	p, closer, err = WrapProvider("record:"+path, live, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Stream(context.Background(), baseRequest("x"), nil)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	p, closer, err = WrapProvider(path, nil, RecorderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if r, err := p.Stream(context.Background(), baseRequest("x"), nil); err != nil || r.Message.Text() != "hi" {
		t.Fatalf("bare path should replay: %v %v", r, err)
	}
}

func TestReadCassetteErrors(t *testing.T) {
	hdr := `{"type":"header","format":"gta-cassette","version":1}` + "\n"
	okIt := `{"type":"interaction","hash":"h","request":{"model":"m"},"response":{"message":{"role":"assistant","blocks":[]},"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"cache_read_tokens":0,"cache_write_tokens":0}}}` + "\n"
	cases := map[string]string{
		"empty":           "",
		"no header":       okIt,
		"bad format":      `{"type":"header","format":"nope","version":1}` + "\n",
		"future version":  `{"type":"header","format":"gta-cassette","version":99}` + "\n",
		"garbage":         hdr + "not json\n",
		"unknown type":    hdr + `{"type":"weird"}` + "\n",
		"no request":      hdr + `{"type":"interaction","response":{"message":{"role":"assistant","blocks":[]}}}` + "\n",
		"both":            hdr + `{"type":"interaction","request":{},"error":"x","response":{"message":{"role":"assistant","blocks":[]}}}` + "\n",
		"neither":         hdr + `{"type":"interaction","request":{}}` + "\n",
		"bad event":       hdr + `{"type":"interaction","request":{},"error":"x","events":[{"type":"zap"}]}` + "\n",
		"trailing object": hdr + `{"type":"interaction","request":{},"error":"x"} {}` + "\n",
	}
	for name, in := range cases {
		if _, err := ReadCassette(strings.NewReader(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	c, err := ReadCassette(strings.NewReader("\n" + hdr + "\n\r\n" + okIt + okIt))
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 || c.Interactions()[1].Seq != 1 {
		t.Fatalf("got %d interactions", c.Len())
	}
	var ce *CassetteError
	_, err = ReadCassette(strings.NewReader(hdr + okIt + "{bad\n"))
	if !errors.As(err, &ce) || ce.Line != 3 {
		t.Errorf("want error on line 3, got %v", err)
	}
}

func FuzzReadCassette(f *testing.F) {
	live := &scriptProvider{name: "p", reply: func(req llm.Request) ([]llm.Event, *llm.Response, error) {
		evs, resp := toolReply("t1", "read_file", map[string]any{"path": "a", "n": 1.5})
		return evs, resp, nil
	}}
	rec, _ := NewRecorder(ModeRecord, nil, live, RecorderOptions{RecordErrors: true})
	rec.Stream(context.Background(), baseRequest("seed"), nil)
	var buf bytes.Buffer
	rec.Cassette().WriteTo(&buf)
	f.Add(buf.Bytes())
	f.Add([]byte(`{"type":"header","format":"gta-cassette","version":1}` + "\n" + `{"type":"interaction","request":{"a":[1,2.0,{"b":null}]},"error":"x"}`))
	f.Add([]byte("{}\n{}\n"))
	f.Add([]byte{0xff, 0xfe, '\n'})
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := ReadCassette(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Anything that parses must re-encode, re-parse to the same
		// number of interactions, hash, and serve replays without panics.
		var out bytes.Buffer
		if _, err := c.WriteTo(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		c2, err := ReadCassette(&out)
		if err != nil {
			t.Fatalf("re-read of re-encoded cassette: %v\n%s", err, out.String())
		}
		if c2.Len() != c.Len() {
			t.Fatalf("interaction count changed: %d -> %d", c.Len(), c2.Len())
		}
		rep, err := NewRecorder(ModeReplay, c2, nil, RecorderOptions{})
		if err != nil {
			return
		}
		_, _ = rep.Stream(context.Background(), baseRequest("seed"), func(llm.Event) {})
	})
}
