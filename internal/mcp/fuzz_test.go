package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

var jsonrpcSeeds = []string{
	`{"jsonrpc":"2.0","id":1,"result":{}}`,
	`{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"nope","data":{"x":1}}}`,
	`{"jsonrpc":"2.0","id":1.0,"result":null}`,
	`{"jsonrpc":"2.0","id":1e0,"result":[]}`,
	`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}]`,
	`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	`{"jsonrpc":"2.0","id":{"a":1},"method":"sampling/createMessage","params":{}}`,
	`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse"}}`,
	`[]`, `[1,"x",null]`, `{}`, `{"id":1}`, `garbage`, ``, `{"id":99999999999999999999,"result":1}`,
}

// FuzzConnHandle feeds arbitrary server payloads to the client endpoint
// while a request is pending. It must never panic or block, and the
// pending request must end with a result or an error.
func FuzzConnHandle(f *testing.F) {
	for _, s := range jsonrpcSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		pt := newPipeTransport()
		c := newConn(time.Second, nil)
		c.onNotify = func(string, json.RawMessage) {}
		c.bind(pt)
		pt.onSend = func(msg []byte) {
			var m wireMessage
			if json.Unmarshal(msg, &m) == nil && m.Method == "tools/list" {
				c.handle(data)
				// Whatever data did, finish the request.
				c.handle([]byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{}}`))
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var out json.RawMessage
		err := c.call(ctx, "tools/list", nil, &out, 0)
		if err != nil && ctx.Err() != nil {
			t.Fatalf("request hung: %v", err)
		}
		c.handle(data)
		c.close()
	})
}

// FuzzIDKey checks id canonicalisation: never a panic, and every integer
// id matches however it is spelled.
func FuzzIDKey(f *testing.F) {
	for _, s := range []string{`1`, `"1"`, `1.0`, `1e0`, `-0`, `null`, `{}`, `"1"`, ` 2 `, `1e400`} {
		f.Add([]byte(s), int64(1))
	}
	f.Fuzz(func(t *testing.T, raw []byte, n int64) {
		_, _ = idKey(raw)
		want := strconv.FormatInt(n, 10)
		for _, spelled := range []string{want, want + ".0", " " + want + "\n"} {
			if k, ok := idKey(json.RawMessage(spelled)); !ok || k != want {
				t.Fatalf("idKey(%q) = %q, %v; want %q", spelled, k, ok, want)
			}
		}
	})
}

// FuzzReadStream feeds arbitrary SSE bodies to the HTTP transport's stream
// reader. It must never panic, must stop at the end of the body, and must
// deliver only well-formed JSON-RPC payloads to the handler.
func FuzzReadStream(f *testing.F) {
	for _, s := range []string{
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n",
		"id: 5\nretry: 10\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"x\"}\r\n\r\n: keepalive\n\n",
		"data: [\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}]\n\n",
		"data:{}\n", "event: other\ndata: {}\n\n", "retry: 99999999999999999999\n\n", "id: a\x00b\n\n", "\n\n\n", "data",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var got [][]byte
		tr := &httpTransport{logf: func(string, ...any) {}, handler: func(b []byte) {
			if !json.Valid(b) {
				t.Fatalf("handler got invalid JSON %q", b)
			}
			got = append(got, b)
		}}
		ok, _, retry, err := tr.readStream(context.Background(), bytes.NewReader(body), "1")
		if !ok && err == nil {
			t.Fatal("stream ended without the response and without an error")
		}
		if retry < 0 {
			t.Fatalf("negative retry %v", retry)
		}
	})
}

// A huge SSE retry hint used to overflow to a negative duration.
func TestReadStreamClampsRetry(t *testing.T) {
	tr := &httpTransport{logf: func(string, ...any) {}, handler: func([]byte) {}}
	_, _, retry, _ := tr.readStream(context.Background(), strings.NewReader("retry: 9999999999999\n\n"), "1")
	if retry != listenMaxBackoff {
		t.Fatalf("retry = %v", retry)
	}
}

// FuzzReadLine compares readLine with a straightforward reference.
func FuzzReadLine(f *testing.F) {
	for _, s := range []string{"a\nb\r\n", "\r\n\n", "x", strings.Repeat("y", 40) + "\nz"} {
		f.Add([]byte(s), 8)
	}
	f.Fuzz(func(t *testing.T, data []byte, max int) {
		if max < 0 || max > 1<<12 {
			return
		}
		r := bufio.NewReaderSize(bytes.NewReader(data), 16)
		lines := bytes.SplitAfter(data, []byte("\n"))
		for _, want := range lines {
			if len(want) == 0 {
				continue
			}
			got, err := readLine(r, max)
			w := trimEOL(want)
			if len(w) > max {
				if err != errLineTooLong && !(err != nil && !bytes.HasSuffix(want, []byte("\n"))) {
					t.Fatalf("line %q (max %d): got %q, %v; want errLineTooLong", want, max, got, err)
				}
				continue
			}
			if err != nil || !bytes.Equal(got, w) {
				t.Fatalf("line %q (max %d): got %q, %v", want, max, got, err)
			}
		}
		if _, err := readLine(r, max); err == nil {
			t.Fatal("read past the end")
		}
	})
}
