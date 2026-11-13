package openai

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var chunkSamples = []string{
	`{"id":"c1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`{"id":"c1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
	`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","system_fingerprint":"fp","choices":[{"index":0,"delta":{"content":"a\nb \"q\" \\ \/ é世 😀 \ud800x \t\r\b\f"},"logprobs":null,"finish_reason":null}],"usage":null}`,
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"think","reasoning":"think"}}]}`,
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":{"a":1}}},{"index":2,"function":{"arguments":null}},{"index":null},{"id":"x"},null,{"index":3},{"index":4},{"index":5}]}}]}`,
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
	`{"choices":null,"error":null}`,
	`{"choices":[null,{"index":1}]}`,
	`{"choices":[{"index":-0,"delta":null}]}`,
	` {"id" : "x" , "choices" : [ { "delta" : { "content" : "y" } } ] } `,
	`{"id":"x","ID":"y"}`,
	`{"id":"x","id":"y"}`,
	`{"Choices":[]}`,
	`{"choices":[{"index":1.5}]}`,
	`{"choices":[{"index":1e2}]}`,
	`{"choices":[{"index":99999999999999999999}]}`,
	`{"choices":[{"delta":{"content":5}}]}`,
	`{"choices":[{"delta":{"content":"bad` + "\xff" + `"}}]}`,
	`{"choices":[{"delta":{"content":"ctl` + "\x01" + `"}}]}`,
	`{"choices":[{"message":{"content":"full"}}]}`,
	`{"x_groq":{"usage":{"prompt_tokens":3}}}`,
	`{"error":{"message":"boom","code":"429"}}`,
	`{"unknown":{"a":[1,2,{"b":null}],"c":true,"d":false,"e":-1.5e+3,"f":"s"}}`,
	`{"unknown":[1,2,}`,
	`{"id":"x"} trailing`,
	`[1,2]`,
	`"str"`,
	`{"id":"A\u00"}`,
	`{"content":"x"}`,
	`{"kKey":"x"}`,
	`{"choices":[{"index":0},{"index":1}]}`,
	`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"n","arguments":"\"}"}}]}}]}`,
	`{}`, ``, `{`, `{"a":}`, `{"a" 1}`, `{"a":01}`, `{"a":-}`, `{"a":1.}`, `{"a":tru}`,
}

func checkChunkDecoder(t *testing.T, data []byte) {
	t.Helper()
	var d chunkDecoder
	got, ok := d.decode(data)
	var want chunk
	err := json.Unmarshal(data, &want)
	if !ok {
		return // falls back to encoding/json
	}
	if err != nil {
		t.Fatalf("fast path accepted %q, encoding/json: %v", data, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("decode(%q)\n got %+v\nwant %+v", data, *got, want)
	}
	// A second decode with the same decoder must not see the first.
	got2, ok2 := d.decode(data)
	if !ok2 || !reflect.DeepEqual(*got2, want) {
		t.Fatalf("second decode of %q differs", data)
	}
}

func TestChunkDecoderMatchesJSON(t *testing.T) {
	fast := 0
	for _, s := range chunkSamples {
		checkChunkDecoder(t, []byte(s))
		var d chunkDecoder
		if _, ok := d.decode([]byte(s)); ok {
			fast++
		}
	}
	if fast < 10 {
		t.Errorf("only %d samples took the fast path", fast)
	}
	// Common shapes must take the fast path.
	for _, s := range chunkSamples[:8] {
		var d chunkDecoder
		if _, ok := d.decode([]byte(s)); !ok {
			t.Errorf("fast path refused %s", s)
		}
	}
}

func TestChunkDecoderReusesStrings(t *testing.T) {
	var d chunkDecoder
	a, _ := d.decode([]byte(chunkSamples[1]))
	id := a.ID
	b, _ := d.decode([]byte(chunkSamples[1]))
	if b.ID != id || b.Choices[0].Delta.Content != "Hello " {
		t.Fatalf("got %+v", b)
	}
}

func FuzzChunkDecoder(f *testing.F) {
	for _, s := range chunkSamples {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkChunkDecoder(t, data)
		checkArgumentText(t, data)
	})
}

func checkArgumentText(t *testing.T, raw []byte) {
	t.Helper()
	got := argumentText(raw)
	// The reference: the original implementation.
	want := func() string {
		raw := bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return ""
		}
		if raw[0] == '"' {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
		}
		return string(raw)
	}()
	if got != want {
		t.Fatalf("argumentText(%q) = %q, want %q", raw, got, want)
	}
}

func TestArgumentText(t *testing.T) {
	for _, s := range []string{`"{\"a\":1}"`, `"plain"`, `{"a":1}`, `null`, ``, `  "x"  `, `"bad`, `"a" "b"`, "\"\xff\"", `"é"`, `"\ud800"`} {
		checkArgumentText(t, []byte(s))
	}
	if got := argumentText([]byte(strings.Repeat(" ", 3) + `"ok"`)); got != "ok" {
		t.Fatal(got)
	}
}
