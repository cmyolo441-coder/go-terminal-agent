package lsp

// A fake language server used by the tests. TestMain runs it when the test
// binary is started with GTA_LSP_FAKE=1, so the manager can launch it as a
// real child process speaking LSP over stdio. Its framing and position
// handling are written independently of the client so the two check each
// other.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
)

const (
	fakeEnv       = "GTA_LSP_FAKE"
	fakeModeEnv   = "GTA_LSP_FAKE_MODE"
	fakeMarkerEnv = "GTA_LSP_FAKE_MARKER"
	fakeDelayEnv  = "GTA_LSP_FAKE_DELAY_MS"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		runFakeServer(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeReadFrame reads one framed message.
func fakeReadFrame(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if n < 0 {
				continue
			}
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, err
			}
		}
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func fakeWriteFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Content-Length: %d\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n%s", len(body), body)
	return err
}

type fakeMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// fakeState is what fake/state reports to tests.
type fakeState struct {
	Methods     []string                   `json:"methods"`
	Replies     map[string]json.RawMessage `json:"replies"`
	Versions    map[string]int             `json:"versions"`
	LanguageIDs map[string]string          `json:"languageIds"`
	RootURI     string                     `json:"rootUri"`
	Encoding    string                     `json:"encoding"`
	InitOptions json.RawMessage            `json:"initOptions"`
	Settings    json.RawMessage            `json:"settings"`
	Cancelled   []string                   `json:"cancelled"`
	Saves       int                        `json:"saves"`
	PID         int                        `json:"pid"`
}

type fakeServer struct {
	mode   map[string]bool
	marker string
	out    io.Writer
	wmu    sync.Mutex

	mu       sync.Mutex
	st       fakeState
	docs     map[string]string
	nextID   int
	outgoing map[string]string // id -> method of our requests
	hangs    map[string]chan struct{}
}

func runFakeServer(in io.Reader, out io.Writer) {
	s := &fakeServer{
		mode:     map[string]bool{},
		marker:   os.Getenv(fakeMarkerEnv),
		out:      out,
		docs:     map[string]string{},
		outgoing: map[string]string{},
		hangs:    map[string]chan struct{}{},
	}
	for _, m := range strings.Split(os.Getenv(fakeModeEnv), ",") {
		if m = strings.TrimSpace(m); m != "" {
			s.mode[m] = true
		}
	}
	s.st = fakeState{Replies: map[string]json.RawMessage{}, Versions: map[string]int{}, LanguageIDs: map[string]string{}, Encoding: EncodingUTF16, PID: os.Getpid()}
	r := bufio.NewReader(in)
	for {
		body, err := fakeReadFrame(r)
		if err != nil {
			return
		}
		var m fakeMsg
		if err := json.Unmarshal(body, &m); err != nil {
			continue
		}
		s.handle(&m)
	}
}

func (s *fakeServer) send(v any) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = fakeWriteFrame(s.out, v)
}

func (s *fakeServer) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *fakeServer) replyErr(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *fakeServer) request(method string, params any) {
	s.mu.Lock()
	s.nextID++
	id := "srv-" + strconv.Itoa(s.nextID)
	s.outgoing[strconv.Quote(id)] = method
	s.mu.Unlock()
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (s *fakeServer) notify(method string, params any) {
	s.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// crash exits the process the first time it is called when the marker
// file does not exist yet (crash once), or always.
func (s *fakeServer) crash(always, once string) {
	if s.mode[always] {
		os.Exit(3)
	}
	if s.mode[once] && s.marker != "" {
		if _, err := os.Stat(s.marker); err != nil {
			_ = os.WriteFile(s.marker, []byte("crashed"), 0o644)
			os.Exit(3)
		}
	}
}

func (s *fakeServer) handle(m *fakeMsg) {
	if m.Method == "" {
		// A response to one of our requests.
		s.mu.Lock()
		method := s.outgoing[string(m.ID)]
		delete(s.outgoing, string(m.ID))
		if method != "" {
			if len(m.Error) > 0 {
				s.st.Replies[method] = append(json.RawMessage(`{"error":`), append(m.Error, '}')...)
			} else {
				res := m.Result
				if len(res) == 0 {
					res = json.RawMessage("null")
				}
				s.st.Replies[method] = append(json.RawMessage(`{"result":`), append(res, '}')...)
			}
		}
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.st.Methods = append(s.st.Methods, m.Method)
	s.mu.Unlock()

	switch m.Method {
	case "initialize":
		if s.mode["init-fail"] {
			fmt.Fprintln(os.Stderr, "fake: cannot start: missing toolchain")
			os.Exit(1)
		}
		if d, _ := strconv.Atoi(os.Getenv(fakeDelayEnv)); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		var p struct {
			RootURI      string          `json:"rootUri"`
			InitOptions  json.RawMessage `json:"initializationOptions"`
			Capabilities struct {
				General struct {
					PositionEncodings []string `json:"positionEncodings"`
				} `json:"general"`
			} `json:"capabilities"`
		}
		_ = json.Unmarshal(m.Params, &p)
		caps := map[string]any{
			"textDocumentSync":   map[string]any{"openClose": true, "change": 1, "save": map[string]any{"includeText": false}},
			"definitionProvider": true,
			"referencesProvider": map[string]any{},
			"hoverProvider":      true,
		}
		if s.mode["incremental"] {
			caps["textDocumentSync"] = 2
		}
		s.mu.Lock()
		s.st.RootURI = p.RootURI
		s.st.InitOptions = p.InitOptions
		if s.mode["utf8"] {
			for _, e := range p.Capabilities.General.PositionEncodings {
				if e == EncodingUTF8 {
					s.st.Encoding = EncodingUTF8
					caps["positionEncoding"] = EncodingUTF8
				}
			}
		}
		s.mu.Unlock()
		if s.mode["pull"] || s.mode["pull-broken"] {
			caps["diagnosticProvider"] = map[string]any{"interFileDependencies": false, "workspaceDiagnostics": false}
		}
		s.reply(m.ID, map[string]any{"capabilities": caps, "serverInfo": map[string]any{"name": "fake"}})
	case "initialized":
		s.request("workspace/configuration", map[string]any{"items": []map[string]any{
			{"section": "fake"}, {"section": "fake.nested.key"}, {"section": "missing"},
		}})
		s.request("window/workDoneProgress/create", map[string]any{"token": "t1"})
		s.request("client/registerCapability", map[string]any{"registrations": []any{}})
		s.request("workspace/applyEdit", map[string]any{"edit": map[string]any{}})
		s.request("fake/unknownRequest", nil)
		s.notify("window/logMessage", map[string]any{"type": 1, "message": "fake server ready"})
	case "workspace/didChangeConfiguration":
		var p struct {
			Settings json.RawMessage `json:"settings"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		s.st.Settings = p.Settings
		s.mu.Unlock()
	case "textDocument/didOpen":
		s.crash("crash-always", "crash-once")
		var p struct {
			TextDocument struct {
				URI        string `json:"uri"`
				LanguageID string `json:"languageId"`
				Version    int    `json:"version"`
				Text       string `json:"text"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		s.docs[p.TextDocument.URI] = p.TextDocument.Text
		s.st.Versions[p.TextDocument.URI] = p.TextDocument.Version
		s.st.LanguageIDs[p.TextDocument.URI] = p.TextDocument.LanguageID
		s.mu.Unlock()
		s.publish(p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.Text)
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Range *json.RawMessage `json:"range"`
				Text  string           `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if len(p.ContentChanges) != 1 || p.ContentChanges[0].Range != nil {
			fmt.Fprintln(os.Stderr, "fake: expected one full-text change")
			os.Exit(4)
		}
		s.mu.Lock()
		if _, ok := s.docs[p.TextDocument.URI]; !ok {
			s.mu.Unlock()
			fmt.Fprintln(os.Stderr, "fake: change before open")
			os.Exit(4)
		}
		if p.TextDocument.Version <= s.st.Versions[p.TextDocument.URI] {
			s.mu.Unlock()
			fmt.Fprintln(os.Stderr, "fake: version did not increase")
			os.Exit(4)
		}
		text := p.ContentChanges[0].Text
		s.docs[p.TextDocument.URI] = text
		s.st.Versions[p.TextDocument.URI] = p.TextDocument.Version
		s.mu.Unlock()
		s.publish(p.TextDocument.URI, p.TextDocument.Version, text)
	case "textDocument/didSave":
		s.mu.Lock()
		s.st.Saves++
		s.mu.Unlock()
	case "textDocument/didClose":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		delete(s.docs, p.TextDocument.URI)
		delete(s.st.Versions, p.TextDocument.URI)
		s.mu.Unlock()
	case "textDocument/diagnostic":
		if s.mode["pull-broken"] {
			s.replyErr(m.ID, CodeMethodNotFound, "no pull here")
			return
		}
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		text := s.docs[p.TextDocument.URI]
		enc := s.st.Encoding
		s.mu.Unlock()
		s.reply(m.ID, map[string]any{"kind": "full", "items": fakeDiagnostics(text, enc)})
	case "textDocument/hover":
		s.crash("crash-hover", "crash-hover-once")
		if s.mode["hang-hover"] {
			ch := make(chan struct{})
			s.mu.Lock()
			s.hangs[string(m.ID)] = ch
			s.mu.Unlock()
			go func() {
				<-ch
				s.replyErr(m.ID, CodeRequestCancelled, "cancelled")
			}()
			return
		}
		ident, _, ok := s.identAt(m.Params)
		if !ok {
			s.reply(m.ID, nil)
			return
		}
		s.reply(m.ID, map[string]any{"contents": map[string]any{"kind": "markdown", "value": "**" + ident + "**\n\nfake hover"}})
	case "textDocument/definition":
		ident, _, ok := s.identAt(m.Params)
		if !ok {
			s.reply(m.ID, nil)
			return
		}
		var locs []Location
		for _, l := range s.occurrences(ident) {
			if l.decl {
				locs = append(locs, l.loc)
			}
		}
		if s.mode["links"] {
			links := make([]LocationLink, len(locs))
			for i, l := range locs {
				links[i] = LocationLink{TargetURI: l.URI, TargetRange: Range{Start: Position{Line: l.Range.Start.Line}, End: l.Range.End}, TargetSelectionRange: l.Range}
			}
			s.reply(m.ID, links)
			return
		}
		if len(locs) == 1 {
			s.reply(m.ID, locs[0])
			return
		}
		s.reply(m.ID, locs)
	case "textDocument/references":
		ident, _, ok := s.identAt(m.Params)
		if !ok {
			s.reply(m.ID, []Location{})
			return
		}
		var p struct {
			Context struct {
				IncludeDeclaration bool `json:"includeDeclaration"`
			} `json:"context"`
		}
		_ = json.Unmarshal(m.Params, &p)
		locs := []Location{}
		for _, l := range s.occurrences(ident) {
			if !l.decl || p.Context.IncludeDeclaration {
				locs = append(locs, l.loc)
			}
		}
		s.reply(m.ID, locs)
	case "fake/state":
		s.mu.Lock()
		data, _ := json.Marshal(s.st)
		s.mu.Unlock()
		s.reply(m.ID, json.RawMessage(data))
	case "$/cancelRequest":
		var p struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		s.st.Cancelled = append(s.st.Cancelled, string(p.ID))
		ch := s.hangs[string(p.ID)]
		delete(s.hangs, string(p.ID))
		s.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	case "shutdown":
		if s.mode["ignore-shutdown"] {
			return
		}
		s.reply(m.ID, nil)
	case "exit":
		os.Exit(0)
	default:
		if len(m.ID) > 0 {
			s.replyErr(m.ID, CodeMethodNotFound, "unknown method "+m.Method)
		}
	}
}

// publish pushes diagnostics for a document unless the mode says not to.
func (s *fakeServer) publish(uri string, version int, text string) {
	if s.mode["silent"] || s.mode["pull"] {
		return
	}
	s.mu.Lock()
	enc := s.st.Encoding
	s.mu.Unlock()
	diags := fakeDiagnostics(text, enc)
	params := map[string]any{"uri": uri, "diagnostics": diags}
	if !s.mode["noversion"] {
		params["version"] = version
	}
	if s.mode["phased"] {
		// Syntax first (warnings only), the full set shortly after.
		var first []map[string]any
		for _, d := range diags {
			if d["severity"] != 1 {
				first = append(first, d)
			}
		}
		early := map[string]any{"uri": uri, "diagnostics": first}
		if !s.mode["noversion"] {
			early["version"] = version
		}
		s.notify("textDocument/publishDiagnostics", early)
		go func() {
			time.Sleep(80 * time.Millisecond)
			s.notify("textDocument/publishDiagnostics", params)
		}()
		return
	}
	if s.mode["stale-first"] {
		// An outdated publish for an older version arrives first.
		old := map[string]any{"uri": uri, "version": version - 1, "diagnostics": []any{}}
		s.notify("textDocument/publishDiagnostics", old)
	}
	s.notify("textDocument/publishDiagnostics", params)
}

// fakeDiagnostics reports ERROR as an error and WARN as a warning wherever
// they occur, with character offsets in enc.
func fakeDiagnostics(text, enc string) []map[string]any {
	out := []map[string]any{}
	for i, line := range strings.Split(text, "\n") {
		for _, w := range []struct {
			word string
			sev  int
			code any
		}{{"ERROR", 1, "E1"}, {"WARN", 2, 42}} {
			idx := strings.Index(line, w.word)
			if idx < 0 {
				continue
			}
			start := fakeUnits(line[:idx], enc)
			out = append(out, map[string]any{
				"range":    map[string]any{"start": map[string]int{"line": i, "character": start}, "end": map[string]int{"line": i, "character": start + len(w.word)}},
				"severity": w.sev,
				"source":   "fake",
				"code":     w.code,
				"message":  "found " + w.word + "\nsecond line of detail",
			})
		}
	}
	return out
}

func fakeUnits(s, enc string) int {
	if enc == EncodingUTF8 {
		return len(s)
	}
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// fakeByteOffset converts a character offset in enc to a byte offset.
func fakeByteOffset(line string, char int, enc string) int {
	if enc == EncodingUTF8 {
		if char > len(line) {
			return len(line)
		}
		return char
	}
	units := 0
	for i, r := range line {
		if units >= char {
			return i
		}
		units += len(utf16.Encode([]rune{r}))
	}
	return len(line)
}

func fakeWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// identAt finds the identifier at a textDocument position.
func (s *fakeServer) identAt(params json.RawMessage) (string, string, bool) {
	var p textDocumentPositionParams
	if json.Unmarshal(params, &p) != nil {
		return "", "", false
	}
	s.mu.Lock()
	text, ok := s.docs[p.TextDocument.URI]
	enc := s.st.Encoding
	s.mu.Unlock()
	if !ok {
		return "", "", false
	}
	lines := strings.Split(text, "\n")
	if p.Position.Line >= len(lines) {
		return "", "", false
	}
	line := lines[p.Position.Line]
	off := fakeByteOffset(line, p.Position.Character, enc)
	runes := []rune(line)
	col := len([]rune(line[:off]))
	start, end := col, col
	for start > 0 && fakeWord(runes[start-1]) {
		start--
	}
	for end < len(runes) && fakeWord(runes[end]) {
		end++
	}
	if start == end {
		return "", "", false
	}
	return string(runes[start:end]), p.TextDocument.URI, true
}

type fakeOcc struct {
	loc  Location
	decl bool
}

// occurrences finds whole-word uses of ident in open documents and in .fk
// files under the root.
func (s *fakeServer) occurrences(ident string) []fakeOcc {
	s.mu.Lock()
	texts := map[string]string{}
	for u, t := range s.docs {
		texts[u] = t
	}
	root := s.st.RootURI
	enc := s.st.Encoding
	s.mu.Unlock()
	if u, err := url.Parse(root); err == nil && u.Path != "" {
		_ = filepath.WalkDir(filepath.FromSlash(u.Path), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".fk" {
				return nil
			}
			uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String()
			if _, open := texts[uri]; !open {
				if data, err := os.ReadFile(p); err == nil {
					texts[uri] = string(data)
				}
			}
			return nil
		})
	}
	uris := make([]string, 0, len(texts))
	for u := range texts {
		uris = append(uris, u)
	}
	sort.Strings(uris)
	var out []fakeOcc
	for _, u := range uris {
		for i, line := range strings.Split(texts[u], "\n") {
			from := 0
			for {
				idx := strings.Index(line[from:], ident)
				if idx < 0 {
					break
				}
				idx += from
				from = idx + len(ident)
				before := []rune(line[:idx])
				after := []rune(line[idx+len(ident):])
				if (len(before) > 0 && fakeWord(before[len(before)-1])) || (len(after) > 0 && fakeWord(after[0])) {
					continue
				}
				start := fakeUnits(line[:idx], enc)
				loc := Location{URI: u, Range: Range{Start: Position{Line: i, Character: start}, End: Position{Line: i, Character: start + fakeUnits(ident, enc)}}}
				out = append(out, fakeOcc{loc: loc, decl: strings.HasSuffix(strings.TrimSpace(line[:idx]), "func")})
			}
		}
	}
	return out
}
