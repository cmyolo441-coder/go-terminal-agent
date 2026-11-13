package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fuzzChain builds a valid three-record chain with seal, as the logger
// writes it.
func fuzzChain(t testing.TB, alg string, key []byte, input string) []byte {
	var out bytes.Buffer
	prev := Genesis
	for i := 1; i <= 3; i++ {
		rec := Record{V: 1, Seq: uint64(i), Time: time.Unix(int64(i), 0).UTC(), Event: EventType("tool_call"),
			Tool: "bash", Input: input, Alg: alg, PrevHash: prev, Details: map[string]string{"k": input}}
		line, sum, err := seal(rec, key)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(line)
		prev = sum
	}
	return out.Bytes()
}

// FuzzVerify checks that verification never panics on arbitrary input,
// and that flipping any bits of any byte of a valid chain is detected.
func FuzzVerify(f *testing.F) {
	key := []byte("fuzz-key")
	f.Add([]byte(nil), 0, byte(1), "ls -la", false)
	f.Add([]byte("{}\n"), 10, byte(0x20), "é \"\\", true)
	f.Add([]byte(`{"seq":1,"alg":"sha256","prev_hash":"x","hash":"`+strings.Repeat("0", 64)+`"}`+"\n"), 200, byte(0x80), "", false)
	f.Add([]byte(`{"seq":0,"alg":"sha256","prev_hash":"x","hash":"`+strings.Repeat("0", 64)+`"}`+"\n"), 7, byte(0x01), "x", true)
	f.Fuzz(func(t *testing.T, junk []byte, pos int, xor byte, input string, hmacKey bool) {
		// Arbitrary input must not panic, keyed or not.
		_, _ = VerifyReader("junk", bytes.NewReader(junk), VerifyOptions{})
		_, _ = VerifyReader("junk", bytes.NewReader(junk), VerifyOptions{HMACKey: key, RequireHMAC: true, AllowPrunedHead: true})

		alg, k := AlgSHA256, []byte(nil)
		opts := VerifyOptions{}
		if hmacKey {
			alg, k = AlgHMACSHA256, key
			opts = VerifyOptions{HMACKey: key, RequireHMAC: true}
		}
		chain := fuzzChain(t, alg, k, input)
		rep, err := VerifyReader("chain", bytes.NewReader(chain), opts)
		if err != nil || !rep.OK() || rep.Records != 3 {
			t.Fatalf("valid chain failed: %+v %v", rep, err)
		}
		if xor == 0 || len(chain) == 0 {
			return
		}
		pos = int(uint(pos) % uint(len(chain)))
		bad := append([]byte(nil), chain...)
		bad[pos] ^= xor
		rep, err = VerifyReader("chain", bytes.NewReader(bad), opts)
		if err == nil && rep.OK() {
			t.Fatalf("flipping byte %d (%q ^ %#x) went unnoticed", pos, chain[pos], xor)
		}
	})
}
