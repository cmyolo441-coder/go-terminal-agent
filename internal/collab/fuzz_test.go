package collab

import (
	"encoding/json"
	"os"
	"testing"
)

var osReadFile = os.ReadFile

func FuzzDecodeClientMessage(f *testing.F) {
	for _, s := range []string{
		`{"type":"heartbeat"}`,
		`{"type":"heartbeat","data":{"vv":{"prompt":{"a":3}}}}`,
		`{"type":"chat","seq":1,"data":{"text":"hi","attachments":[{"name":"a","data":"AAEC"}]}}`,
		`{"type":"presence","data":{"cursor":3,"typing":true,"selection":{"anchor":1,"head":2}}}`,
		`{"type":"ops","data":{"doc":"prompt","ops":[{"id":{"r":"p","s":1},"k":"ins","l":1,"t":"x"}]}}`,
		`{"type":"ops","data":{"doc":"context","ops":[{"id":{"r":"p","s":2},"k":"rem","e":"x","tags":[{"r":"p","s":1}]}]}}`,
		`{"type":"ops","data":{"doc":"settings","ops":[{"id":{"r":"p","s":1},"key":"k","v":1,"ts":{"w":5,"n":"p"}}]}}`,
		`{"type":"sync","data":{"doc":"prompt","vv":{"x":1},"reset":true}}`,
		`{"type":"vote","data":{"approval_id":"a1","approve":true}}`,
		`{"type":"set_role","data":{"participant":"p","role":"editor"}}`,
		`{"type":"kick","data":{"participant":"p"}}`,
		`{"type":"submit"}`, `{"type":"leave"}`, `{}`, `[]`, `{"type":"chat","data":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, payload, err := DecodeClientMessage(data)
		if err != nil {
			return
		}
		// Whatever decodes must re-encode and decode to the same type.
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		m2, _, err := DecodeClientMessage(raw)
		if err != nil || m2.Type != m.Type {
			t.Fatalf("round trip: %v", err)
		}
		if o, ok := payload.(*OpsMessage); ok {
			ops, err := decodeOps(o.Doc, o.Ops)
			if err != nil {
				t.Fatal(err)
			}
			// Applying validated operations must never panic.
			switch v := ops.(type) {
			case []TextOp:
				x := NewText("fuzz")
				_, _ = x.Insert(0, "seed")
				_, _ = x.Apply(v...)
				_ = x.String()
				x.CollectGarbage(nil)
			case []SetOp:
				_, _ = NewORSet("fuzz").Apply(v...)
			case []MapOp:
				_, _ = NewLWWMap("fuzz", nil).Apply(v...)
			}
		}
	})
}

func FuzzTextSnapshot(f *testing.F) {
	x := NewText("a")
	_, _ = x.Insert(0, "hello")
	_, _ = x.Delete(1, 2)
	seed, _ := json.Marshal(x.Snapshot(true))
	f.Add(seed)
	f.Add([]byte(`{"vv":{},"runs":[{"id":{"l":1,"r":"a"},"n":2,"t":"ab","s":1}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s TextSnapshot
		if json.Unmarshal(data, &s) != nil {
			return
		}
		y, err := RestoreText("b", s)
		if err != nil {
			return
		}
		checkTextInvariants(t, y)
		if y.Len() > 0 {
			_, _ = y.Delete(0, 1)
		}
		_, _ = y.Insert(0, "z")
		checkTextInvariants(t, y)
	})
}
