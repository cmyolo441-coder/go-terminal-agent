package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// FuzzStream feeds arbitrary SSE bodies through the stream parser and the
// reply accumulator, as Provider.read does. It must never panic, and every
// tool call it assembles must carry a JSON object as input, which is what
// tools and every provider (on replay or fallback) require.
func FuzzStream(f *testing.F) {
	for _, s := range []string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"pa\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"th\\\":\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"b\",\"function\":{\"name\":\"t\",\"arguments\":{\"k\":1}}}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"t\",\"arguments\":\"[1,2]\"}}]},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"t\",\"arguments\":\"\\\"s\\\"\"}}]},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"r\",\"refusal\":\"no\"}}],\"usage\":{\"prompt_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":9}}}\n\n",
		"data: {\"error\":{\"message\":\"boom\",\"code\":\"429\"}}\n\n",
		"data: {\"a\":\ndata: 1}\ndata: {\"b\":2}\n", ": keep\r\n\r\ndata:[DONE]", "data: {", "\n\n\n",
	} {
		f.Add([]byte(s))
	}
	p := &Provider{opts: Options{Name: "fuzz"}}
	f.Fuzz(func(t *testing.T, body []byte) {
		acc := newAccumulator(func(llm.Event) {})
		done := false
		err := readSSE(bytes.NewReader(body), func() {}, func(data []byte) (bool, error) {
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				done = true
				return true, nil
			}
			var c chunk
			if err := json.Unmarshal(data, &c); err != nil {
				return true, err
			}
			if err := p.streamError(&c, "m"); err != nil {
				return true, err
			}
			acc.addChunk(&c)
			return false, nil
		})
		if err != nil {
			return
		}
		_ = done
		resp, err := acc.result("fuzz", "m")
		if err != nil {
			return
		}
		for _, b := range resp.Message.Blocks {
			if b.Type != llm.BlockToolUse {
				continue
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(b.Input, &obj); err != nil || obj == nil {
				t.Fatalf("tool input %q is not a JSON object", b.Input)
			}
			if b.ToolUseID == "" || b.ToolName == "" {
				t.Fatalf("tool call without id or name: %+v", b)
			}
		}
		if resp.Usage.InputTokens < 0 || resp.Usage.CacheReadTokens < 0 {
			t.Fatalf("negative usage %+v", resp.Usage)
		}
	})
}

// FuzzReadSSE checks the SSE reader on its own: events come out in order
// with their data intact, whether or not the server separates them with
// blank lines, and a large event is refused rather than buffered forever.
func FuzzReadSSE(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `x`, "", `[DONE]`, `{"b":` + "\n" + `2}`} {
		f.Add(s, `{"c":3}`, true)
	}
	f.Fuzz(func(t *testing.T, a, b string, blank bool) {
		if strings.ContainsAny(a+b, "\r\n") || a == "" || b == "" {
			return
		}
		sep := "\n"
		if blank {
			sep = "\n\n"
		}
		body := "data: " + a + sep + "data: " + b + "\n\n"
		var got []string
		err := readSSE(strings.NewReader(body), func() {}, func(d []byte) (bool, error) {
			got = append(got, string(d))
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{a, b}
		if !blank && !json.Valid([]byte(a)) {
			want = []string{a + "\n" + b}
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("body %q: got %q want %q", body, got, want)
		}
	})
}

// FuzzToolInput checks that assembled arguments always become a JSON
// object, and that a valid object is kept (compacted).
func FuzzToolInput(f *testing.F) {
	for _, s := range []string{`{"a":1}`, ` { "a" : [1, 2] } `, `[1]`, `"x"`, `null`, ``, `{"a":`, `{"a":1}{"b":2}`, `1e999`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, args string) {
		in := toolInput(args)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(in, &obj); err != nil || obj == nil {
			t.Fatalf("toolInput(%q) = %q, not an object", args, in)
		}
		var want map[string]json.RawMessage
		if json.Unmarshal([]byte(args), &want) == nil && want != nil {
			var buf bytes.Buffer
			json.Compact(&buf, []byte(strings.TrimSpace(args)))
			if string(in) != buf.String() {
				t.Fatalf("toolInput(%q) = %q, want %q", args, in, buf.String())
			}
		}
		_ = argumentText(json.RawMessage(args))
	})
}
