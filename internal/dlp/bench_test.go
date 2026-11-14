package dlp

import (
	"io"
	"math/rand"
	"strings"
	"testing"
)

var cleanWords = strings.Fields(`the quick brown fox jumps over lazy dog func main return if err != nil
	package import struct interface map string int64 for range context deadline server handler request
	response build version release config value index buffer stream token model agent tool output`)

func benchText(n int, mixed bool) string {
	rng := rand.New(rand.NewSource(42))
	pii := []string{"4111 1111 1111 1111", "jane.doe@corp.com", "+44 20 7946 0958", "536-22-1234",
		"DE89 3704 0044 0532 0130 00", "8.8.8.8", "ABCPE1234F", "DOB: 12/05/1985", "MRN: 00123456"}
	var b strings.Builder
	for b.Len() < n {
		switch {
		case mixed && rng.Intn(60) == 0:
			b.WriteString(pii[rng.Intn(len(pii))])
		case rng.Intn(25) == 0:
			b.WriteString("x := 42; y[i] = 3.14\n")
		default:
			b.WriteString(cleanWords[rng.Intn(len(cleanWords))])
		}
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

func benchProcess(b *testing.B, mixed bool) {
	e := mustEngine(b, nil)
	v, _ := NewVault(nil, 0)
	text := benchText(1<<20, mixed)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Process(text, v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProcessClean1MB(b *testing.B) { benchProcess(b, false) }
func BenchmarkProcessMixed1MB(b *testing.B) { benchProcess(b, true) }

func BenchmarkStreamWriterMixed1MB(b *testing.B) {
	e := mustEngine(b, nil)
	text := []byte(benchText(1<<20, true))
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := e.NewWriter(io.Discard, StreamOptions{})
		for off := 0; off < len(text); off += 4096 {
			w.Write(text[off:min(len(text), off+4096)])
		}
		w.Close()
	}
}

func BenchmarkAhoCorasick1MB(b *testing.B) {
	rng := rand.New(rand.NewSource(3))
	pats := make([]string, 5000)
	for i := range pats {
		w := make([]byte, 5+rng.Intn(8))
		for j := range w {
			w[j] = byte('a' + rng.Intn(26))
		}
		pats[i] = string(w)
	}
	ac, _ := NewAhoCorasick(pats, ACOptions{CaseInsensitive: true})
	text := benchText(1<<20, false)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ac.Iterate(text, func(ACMatch) bool { return true })
	}
}
