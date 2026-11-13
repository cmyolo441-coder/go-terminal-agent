package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConfigValidate(t *testing.T) {
	good := Config{
		"local":  {Command: "npx", Args: []string{"-y", "server"}, Env: map[string]string{"TOKEN": "${T}"}},
		"remote": {URL: "https://mcp.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${T}"}, TimeoutSec: 120},
		"tmpl":   {URL: "${MCP_URL}"},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]struct {
		name string
		cfg  ServerConfig
		want string
	}{
		"name":        {"has space", ServerConfig{Command: "x"}, "invalid MCP server name"},
		"long name":   {strings.Repeat("a", 65), ServerConfig{Command: "x"}, "invalid MCP server name"},
		"neither":     {"s", ServerConfig{}, "set command"},
		"both":        {"s", ServerConfig{Command: "x", URL: "http://h"}, "not both"},
		"scheme":      {"s", ServerConfig{URL: "ftp://h"}, "http or https"},
		"no host":     {"s", ServerConfig{URL: "http://"}, "no host"},
		"args on url": {"s", ServerConfig{URL: "http://h", Args: []string{"a"}}, "only to command"},
		"hdr on cmd":  {"s", ServerConfig{Command: "x", Headers: map[string]string{"A": "b"}}, "only to url"},
		"bad header":  {"s", ServerConfig{URL: "http://h", Headers: map[string]string{"Bad Header": "v"}}, "invalid header name"},
		"crlf":        {"s", ServerConfig{URL: "http://h", Headers: map[string]string{"X": "a\r\nInjected: 1"}}, "line break"},
		"managed":     {"s", ServerConfig{URL: "http://h", Headers: map[string]string{"Mcp-Session-Id": "x"}}, "managed by the client"},
		"env name":    {"s", ServerConfig{Command: "x", Env: map[string]string{"1BAD": "v"}}, "invalid env variable"},
		"negative":    {"s", ServerConfig{Command: "x", TimeoutSec: -1}, "must not be negative"},
		"too long":    {"s", ServerConfig{Command: "x", TimeoutSec: 99999}, "at most 3600"},
	}
	for label, tc := range cases {
		err := tc.cfg.Validate(tc.name)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", label, err, tc.want)
		}
	}
	// All problems are reported together.
	err := Config{"a b": {}, "c": {Command: "x", URL: "http://h"}}.Validate()
	if err == nil || !strings.Contains(err.Error(), `"a b"`) || !strings.Contains(err.Error(), `"c"`) {
		t.Fatalf("joined err = %v", err)
	}
	var round Config
	b, _ := json.Marshal(good)
	if err := json.Unmarshal(b, &round); err != nil || round["remote"].TimeoutSec != 120 || round["local"].Args[1] != "server" {
		t.Fatalf("JSON round trip: %v %+v", err, round)
	}
	if !strings.Contains(string(b), `"timeout_sec":120`) || strings.Contains(string(b), `"disabled"`) {
		t.Fatalf("JSON tags: %s", b)
	}
}

func TestExpandVars(t *testing.T) {
	env := map[string]string{"A": "alpha", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	for in, want := range map[string]string{
		"plain":            "plain",
		"${A}":             "alpha",
		"x-${A}-y":         "x-alpha-y",
		"${B:-def}":        "def",
		"${EMPTY:-def}":    "def",
		"${EMPTY}":         "",
		"$A stays":         "$A stays",
		"cost $$5":         "cost $5",
		"trailing $":       "trailing $",
		"Bearer ${A}${A}":  "Bearer alphaalpha",
		"${B:-}":           "",
		"${B:-a:-b}":       "a:-b",
		"${A} and ${B:-z}": "alpha and z",
	} {
		got, err := expandVars(in, lookup)
		if err != nil || got != want {
			t.Errorf("expandVars(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"${B}", "${", "${1X}", "${A"} {
		if _, err := expandVars(bad, lookup); err == nil {
			t.Errorf("expandVars(%q) succeeded", bad)
		}
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://user:pass@h.example.com/mcp?token=abc#frag")
	if strings.Contains(got, "pass") || strings.Contains(got, "abc") || !strings.Contains(got, "h.example.com/mcp") {
		t.Fatalf("redactURL = %q", got)
	}
}

func TestToolName(t *testing.T) {
	if got := ToolName("github", "create_issue"); got != "mcp__github__create_issue" {
		t.Fatalf("got %q", got)
	}
	if got := ToolName("my.srv", "do thing/now"); got != "mcp__my_srv__do_thing_now" {
		t.Fatalf("got %q", got)
	}
	long1 := ToolName("server", strings.Repeat("x", 80)+"1")
	long2 := ToolName("server", strings.Repeat("x", 80)+"2")
	if len(long1) != MaxToolNameLen || len(long2) != MaxToolNameLen || long1 == long2 {
		t.Fatalf("long names %q %q", long1, long2)
	}
	if ToolName("s", "") != "mcp__s___" {
		t.Fatalf("empty tool name = %q", ToolName("s", ""))
	}
	used := map[string]bool{"a": true, "a_2": true}
	if got := uniqueName("a", used); got != "a_3" {
		t.Fatalf("uniqueName = %q", got)
	}
	full := strings.Repeat("y", MaxToolNameLen)
	if got := uniqueName(full, map[string]bool{full: true}); len(got) != MaxToolNameLen || !strings.HasSuffix(got, "_2") {
		t.Fatalf("uniqueName long = %q", got)
	}
}

func TestSchemaParts(t *testing.T) {
	schema := `{
	  "type": "object",
	  "properties": {
	    "owner": {"$ref": "#/$defs/Name"},
	    "tree": {"$ref": "#/definitions/Node"},
	    "tags": {"type": "array", "items": {"$ref": "#/$defs/Name"}},
	    "dangling": {"$ref": "#/$defs/Missing", "description": "kept"}
	  },
	  "required": ["tree", "owner", 7],
	  "$defs": {"Name": {"type": "string", "minLength": 1}},
	  "definitions": {"Node": {"type": "object", "properties": {"child": {"$ref": "#/definitions/Node"}}}}
	}`
	props, req := schemaParts(json.RawMessage(schema))
	if strings.Join(req, ",") != "owner,tree" {
		t.Fatalf("required = %v", req)
	}
	owner := props["owner"].(map[string]any)
	if owner["type"] != "string" {
		t.Fatalf("owner = %v", owner)
	}
	items := props["tags"].(map[string]any)["items"].(map[string]any)
	if items["type"] != "string" {
		t.Fatalf("items = %v", items)
	}
	if d := props["dangling"].(map[string]any); d["description"] != "kept" || len(d) != 1 {
		t.Fatalf("dangling = %v", d)
	}
	// The recursive definition is cut off at a finite depth.
	b, err := json.Marshal(props["tree"])
	if err != nil || strings.Contains(string(b), "$ref") || strings.Count(string(b), "child") != maxSchemaDepth {
		t.Fatalf("tree = %s (%v)", b, err)
	}
	for _, raw := range []string{"", "null", "[]", `{"type":"object"}`, "not json"} {
		p, r := schemaParts(json.RawMessage(raw))
		if p == nil || len(p) != 0 || r != nil {
			t.Errorf("schemaParts(%q) = %v %v", raw, p, r)
		}
	}
}

func TestToolResultText(t *testing.T) {
	r := ToolResult{StructuredContent: json.RawMessage(`{"temp":21}`)}
	if r.Text() != `{"temp":21}` {
		t.Fatalf("structured = %q", r.Text())
	}
	r = ToolResult{Content: []Content{{Type: "text", Text: "a"}, {Type: "audio", MimeType: "audio/wav", Data: strings.Repeat("A", 4000)}, {Type: "video"}, {Type: "resource"}}}
	want := "a\n[audio: audio/wav, 2.9 KB]\n[unsupported content type \"video\"]\n[resource: (empty)]"
	if r.Text() != want {
		t.Fatalf("got %q want %q", r.Text(), want)
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("héllo wörld ", 200)
	out := truncate(s, 101)
	if !utf8.ValidString(out) || !strings.Contains(out, "characters omitted") {
		t.Fatalf("truncate = %q", out)
	}
	if truncate("short", 100) != "short" || truncate("x", 0) != "x" {
		t.Fatal("short strings must be unchanged")
	}
	if c := clip("ééé", 3); !utf8.ValidString(c) {
		t.Fatalf("clip = %q", c)
	}
}

func TestRingBuffer(t *testing.T) {
	r := newRingBuffer(16)
	r.Write([]byte("line one\n"))
	if r.String() != "line one\n" {
		t.Fatalf("got %q", r.String())
	}
	r.Write([]byte("line two\nline three\n"))
	got := r.String()
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "line three\n") || len(got) > 16+len("…") {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte(strings.Repeat("z", 100)))
	if !strings.HasSuffix(r.String(), strings.Repeat("z", 15)) {
		t.Fatalf("big write: %q", r.String())
	}
	r2 := newRingBuffer(1024)
	r2.Write([]byte("a\nb\nc\nd\n"))
	if r2.Tail(2) != "…\nc\nd" {
		t.Fatalf("tail = %q", r2.Tail(2))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				r.Write([]byte("concurrent\n"))
				_ = r.String()
			}
		}()
	}
	wg.Wait()
}

func TestReadLine(t *testing.T) {
	in := "short\r\n" + strings.Repeat("x", 100) + "\nnext\nlast-no-newline"
	r := bufio.NewReaderSize(strings.NewReader(in), 16)
	line, err := readLine(r, 50)
	if err != nil || string(line) != "short" {
		t.Fatalf("1: %q %v", line, err)
	}
	if _, err := readLine(r, 50); !errors.Is(err, errLineTooLong) {
		t.Fatalf("2: %v", err)
	}
	if line, err := readLine(r, 50); err != nil || string(line) != "next" {
		t.Fatalf("3: %q %v", line, err)
	}
	if line, err := readLine(r, 50); err != nil || string(line) != "last-no-newline" {
		t.Fatalf("4: %q %v", line, err)
	}
	if _, err := readLine(r, 50); err == nil {
		t.Fatal("5: expected EOF")
	}
}

func TestIDKey(t *testing.T) {
	for raw, want := range map[string]string{`5`: "5", `5.0`: "5", `"5"`: "5", `"abc"`: "abc", ` 7 `: "7"} {
		if got, ok := idKey(json.RawMessage(raw)); !ok || got != want {
			t.Errorf("idKey(%s) = %q %v", raw, got, ok)
		}
	}
	for _, raw := range []string{``, `null`, `{}`} {
		if _, ok := idKey(json.RawMessage(raw)); ok {
			t.Errorf("idKey(%s) ok", raw)
		}
	}
}

// pipeTransport is an in-memory transport whose server side is a function.
type pipeTransport struct {
	mu      sync.Mutex
	sent    [][]byte
	onSend  func(msg []byte)
	doneCh  chan struct{}
	failErr error
	once    sync.Once
}

func newPipeTransport() *pipeTransport { return &pipeTransport{doneCh: make(chan struct{})} }

func (p *pipeTransport) start() error { return nil }
func (p *pipeTransport) send(ctx context.Context, msg []byte) error {
	p.mu.Lock()
	p.sent = append(p.sent, append([]byte(nil), msg...))
	f := p.onSend
	p.mu.Unlock()
	if f != nil {
		go f(msg)
	}
	return nil
}
func (p *pipeTransport) close() error          { p.once.Do(func() { close(p.doneCh) }); return nil }
func (p *pipeTransport) done() <-chan struct{} { return p.doneCh }
func (p *pipeTransport) err() error            { return p.failErr }
func (p *pipeTransport) die(err error)         { p.failErr = err; p.close() }
func (p *pipeTransport) messages() (out []wireMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.sent {
		var m wireMessage
		json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}

func TestConnCorrelationAndBatches(t *testing.T) {
	c := newConn(5*time.Second, t.Logf)
	p := newPipeTransport()
	c.bind(p)
	defer c.close()
	// The server answers with string ids, in a batch, in reverse order, and
	// throws in a stray response and garbage.
	var mu sync.Mutex
	var ids []string
	p.onSend = func(msg []byte) {
		var m wireMessage
		json.Unmarshal(msg, &m)
		mu.Lock()
		ids = append(ids, string(m.ID))
		ready := len(ids) == 2
		batch := `[{"jsonrpc":"2.0","id":"` + strings.Trim(ids[len(ids)-1], `"`) + `","result":{"n":"second"}}`
		if ready {
			batch += `,{"jsonrpc":"2.0","id":` + ids[0] + `,"result":{"n":"first"}}`
		}
		mu.Unlock()
		if !ready {
			return
		}
		c.handle([]byte(`not json`))
		c.handle([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}`))
		c.handle([]byte(batch + "]"))
	}
	results := make(chan string, 2)
	for range 2 {
		go func() {
			var out struct{ N string }
			if err := c.call(context.Background(), "x", nil, &out, 0); err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- out.N
		}()
		time.Sleep(20 * time.Millisecond)
	}
	got := []string{<-results, <-results}
	if !(got[0] == "first" && got[1] == "second" || got[0] == "second" && got[1] == "first") {
		t.Fatalf("results = %v", got)
	}
}

func TestConnServerRequestsAndNotifications(t *testing.T) {
	c := newConn(time.Second, t.Logf)
	p := newPipeTransport()
	c.bind(p)
	defer c.close()
	notes := make(chan string, 2)
	c.onNotify = func(method string, _ json.RawMessage) { notes <- method }
	c.handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage","params":{}}`))
	c.handle([]byte(`{"jsonrpc":"2.0","id":"p","method":"ping"}`))
	c.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`))
	if m := <-notes; m != "notifications/progress" {
		t.Fatalf("notification = %q", m)
	}
	waitFor(t, func() bool { return len(p.messages()) == 2 })
	for _, m := range p.messages() {
		switch string(m.ID) {
		case "1":
			if m.Error == nil || m.Error.Code != CodeMethodNotFound || !strings.Contains(m.Error.Message, "sampling/createMessage") {
				t.Errorf("reply = %+v", m)
			}
		case `"p"`:
			if m.Error != nil || string(m.Result) != "{}" {
				t.Errorf("ping reply = %+v", m)
			}
		default:
			t.Errorf("unexpected reply %+v", m)
		}
	}
}

func TestConnTransportDeathFailsPending(t *testing.T) {
	c := newConn(10*time.Second, t.Logf)
	p := newPipeTransport()
	c.bind(p)
	defer c.close()
	errc := make(chan error, 1)
	go func() { errc <- c.call(context.Background(), "x", nil, nil, 0) }()
	waitFor(t, func() bool { return len(p.messages()) == 1 })
	p.die(errors.New("pipe exploded"))
	select {
	case err := <-errc:
		if err == nil || err.Error() != "pipe exploded" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call not failed")
	}
	if err := c.call(context.Background(), "y", nil, nil, 0); err == nil || err.Error() != "pipe exploded" {
		t.Fatalf("call on dead conn = %v", err)
	}
	if err := c.notify(context.Background(), "n", nil); err == nil {
		t.Fatal("notify on dead conn succeeded")
	}
}

func TestRPCErrorMessage(t *testing.T) {
	e := &RPCError{Code: -32000, Message: "boom", Data: json.RawMessage(`{"detail":"x"}`)}
	if got := e.Error(); got != `mcp: server error -32000: boom ({"detail":"x"})` {
		t.Fatalf("got %q", got)
	}
}
