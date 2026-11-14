package sentinel

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// ---- primitives

func TestFingerprintNormalises(t *testing.T) {
	a := fingerprint("bash", lowerInto(nil, `  LS   -la "src"  `))
	b := fingerprint("bash", lowerInto(nil, `ls -la src`))
	c := fingerprint("bash", lowerInto(nil, `ls -la srcs`))
	d := fingerprint("read", lowerInto(nil, `ls -la src`))
	if a != b {
		t.Fatal("whitespace/case/quote variants differ")
	}
	if a == c || a == d {
		t.Fatal("distinct calls collide")
	}
}

func TestSimHashSeparatesNearFromFar(t *testing.T) {
	base := "go test ./internal/agent/... -run TestRunTools -count=1 -race -timeout 120s"
	near := "go test ./internal/agent/... -run TestRunTools -count=1 -race -timeout 120s -v"
	far := "grep -rn TODO internal cmd docs --include=*.go"
	hb, nb := simhash("bash", lowerInto(nil, base))
	hn, _ := simhash("bash", lowerInto(nil, near))
	hf, _ := simhash("bash", lowerInto(nil, far))
	if nb < 10 {
		t.Fatalf("tokens = %d", nb)
	}
	if d := hamming(hb, hn); d > 6 {
		t.Errorf("near distance %d, want small", d)
	}
	if d := hamming(hb, hf); d < 16 {
		t.Errorf("far distance %d, want large", d)
	}
}

func TestEWM(t *testing.T) {
	e := EWM{Alpha: 0.1}
	for i := 0; i < 500; i++ {
		e.Update(10 + float64(i%2)*2) // 10, 12, 10, 12 … mean 11, sd ≈ 1
	}
	if math.Abs(e.Mean-11) > 0.2 || math.Abs(e.Std()-1) > 0.2 {
		t.Fatalf("mean %v std %v", e.Mean, e.Std())
	}
	if z := e.Z(20, 0.5); z < 7 {
		t.Fatalf("z = %v", z)
	}
}

func TestHoltTracksTrend(t *testing.T) {
	h := Holt{Alpha: 0.5, Beta: 0.3, Res: EWM{Alpha: 0.2}}
	for i := 0; i < 50; i++ {
		h.Update(float64(3 * i))
	}
	if math.Abs(h.Trend-3) > 0.1 || math.Abs(h.Forecast()-150) > 1 {
		t.Fatalf("trend %v forecast %v", h.Trend, h.Forecast())
	}
	if z := h.Z(150, 1); math.Abs(z) > 1 {
		t.Fatalf("on-trend value z = %v", z)
	}
}

func TestRegression(t *testing.T) {
	r := sampleRing{size: 8}
	for i := 0; i < 20; i++ {
		r.push(int64(i)*int64(time.Second), 5+2*float64(i))
	}
	slope, r2, span, ok := r.fit()
	if !ok || math.Abs(slope-2) > 1e-9 || r2 < 0.999 || span != int64(7*time.Second) {
		t.Fatalf("slope %v r2 %v span %v ok %v", slope, r2, span, ok)
	}
}

func TestShannon(t *testing.T) {
	if h := shannon([]byte(strings.Repeat("a", 100))); h != 0 {
		t.Fatalf("constant entropy %v", h)
	}
	buf := make([]byte, 1<<14)
	for i := range buf {
		buf[i] = byte(i)
	}
	if h := shannon(buf); math.Abs(h-8) > 1e-9 {
		t.Fatalf("uniform entropy %v", h)
	}
}

// ---- loop

func TestLoopExactRepeats(t *testing.T) {
	h := newHarness(t, only(DetectorLoop))
	for i := 0; i < 3; i++ {
		h.at(5 * time.Second)
		if v := h.exec("go test ./..."); v.Decision != DecisionAllow {
			t.Fatalf("repeat %d: %v", i+1, v)
		}
	}
	h.at(5 * time.Second)
	v := h.exec(`go   test  "./..."`) // same call, different spelling
	if v.Detector != DetectorLoop {
		t.Fatalf("4th repeat verdict %v", v)
	}
	inc := h.wantIncidents(DetectorLoop, 1)[0]
	if inc.Severity != SeverityMedium || inc.Rule != "exact" || inc.Metrics["identical"] != 4 {
		t.Fatalf("incident %+v", inc)
	}
	matched := 0
	for _, e := range inc.Evidence {
		if e.Matched {
			matched++
		}
	}
	if matched != 4 {
		t.Fatalf("matched evidence %d, want 4", matched)
	}
	for i := 0; i < 4; i++ {
		h.at(5 * time.Second)
		h.exec("go test ./...")
	}
	if got := h.incidents(DetectorLoop); got[len(got)-1].Severity != SeverityHigh {
		t.Fatalf("no escalation to high: %+v", got)
	}
}

func TestLoopFalsePositives(t *testing.T) {
	h := newHarness(t, only(DetectorLoop))
	// Edit-test cycles are progress: a write between runs resets the count.
	for i := 0; i < 10; i++ {
		h.at(5 * time.Second)
		h.exec("go test ./...")
		h.write("main.go", []byte(fmt.Sprintf("package main // %d", i)))
	}
	// Paging through a file with distinct offsets is not a near-duplicate.
	for i := 0; i < 20; i++ {
		h.at(2 * time.Second)
		h.exec(fmt.Sprintf("sed -n %d,%dp internal/agent/agent.go", i*50+1, i*50+50))
	}
	// Repeats spread beyond the window do not accumulate.
	for i := 0; i < 6; i++ {
		h.at(4 * time.Minute)
		h.exec("git status")
	}
	h.wantIncidents(DetectorLoop, 0)
}

func TestLoopNearDuplicates(t *testing.T) {
	h := newHarness(t, only(DetectorLoop))
	flags := []string{"", " -v", " -count=1", " -v -count=1", " -short", " -failfast"}
	for i, f := range flags {
		h.at(3 * time.Second)
		h.exec("go test ./internal/agent/... -run TestRunTools -race -timeout 120s" + f)
		if i < len(flags)-1 && len(h.incidents(DetectorLoop)) > 0 {
			t.Fatalf("flagged after %d calls", i+1)
		}
	}
	inc := h.wantIncidents(DetectorLoop, 1)[0]
	if inc.Rule != "near" || inc.Severity != SeverityLow {
		t.Fatalf("incident %+v", inc)
	}
}

// ---- oscillation

func TestOscillation(t *testing.T) {
	h := newHarness(t, only(DetectorOscillation))
	a, b := []byte("version A"), []byte("version B")
	seq := [][]byte{a, b, a, b, a}
	for _, p := range seq {
		h.at(10 * time.Second)
		h.write("/w/config.yaml", p)
	}
	inc := h.wantIncidents(DetectorOscillation, 1)[0]
	if inc.Severity != SeverityMedium || inc.Metrics["reversions"] != 2 {
		t.Fatalf("incident %+v", inc)
	}
	// Digests from results work too and escalate.
	for _, d := range []uint64{fnv64b(b), fnv64b(a)} {
		h.at(10 * time.Second)
		h.obs(Event{Kind: KindToolResult, Tool: "edit", Target: "/w/config.yaml", Digest: d})
	}
	if got := h.incidents(DetectorOscillation); got[len(got)-1].Severity != SeverityHigh {
		t.Fatalf("no escalation: %+v", got)
	}
}

func TestOscillationFalsePositives(t *testing.T) {
	h := newHarness(t, only(DetectorOscillation))
	for i := 0; i < 20; i++ { // steady forward progress
		h.at(10 * time.Second)
		h.write("/w/main.go", []byte(fmt.Sprintf("v%d", i)))
	}
	for i := 0; i < 5; i++ { // identical rewrites
		h.write("/w/same.go", []byte("same"))
	}
	// A single revert is legitimate; one per file across many files too.
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("/w/f%d.go", i)
		h.write(p, []byte("a"))
		h.write(p, []byte("b"))
		h.write(p, []byte("a"))
	}
	// Reverts spread beyond the window are forgotten.
	for _, c := range []string{"x", "y", "x", "y", "x"} {
		h.at(20 * time.Minute)
		h.write("/w/slow.go", []byte(c))
	}
	h.wantIncidents(DetectorOscillation, 0)
}

// ---- rate

func steadyCalls(h *harness, buckets, perBucket int) {
	for b := 0; b < buckets; b++ {
		for i := 0; i < perBucket; i++ {
			h.at(10 * time.Second / time.Duration(perBucket))
			h.read(fmt.Sprintf("/w/f%d_%d.go", b, i))
		}
	}
}

func TestRateAnomaly(t *testing.T) {
	h := newHarness(t, only(DetectorRate))
	steadyCalls(h, 30, 2)
	h.wantIncidents(DetectorRate, 0)
	// A burst of 40 calls in one bucket.
	var v Verdict
	for i := 0; i < 40; i++ {
		h.at(100 * time.Millisecond)
		if vv := h.read(fmt.Sprintf("/w/burst%d", i)); vv.Detector == DetectorRate {
			v = vv
		}
	}
	inc := h.wantIncidents(DetectorRate, 1)[0]
	if inc.Rule != "calls" || inc.Metrics["z"] < 4 {
		t.Fatalf("incident %+v", inc)
	}
	if v.Decision != DecisionWarn || h.s.Posture(h.sess).Action != ActionThrottle {
		t.Fatalf("verdict %v posture %+v", v, h.s.Posture(h.sess))
	}
}

func TestRateHysteresisAndIdle(t *testing.T) {
	h := newHarness(t, only(DetectorRate))
	steadyCalls(h, 30, 2)
	// An idle hour is skipped rather than averaged in as zeros, so normal
	// work afterwards is not anomalous.
	h.at(time.Hour)
	steadyCalls(h, 10, 2)
	h.wantIncidents(DetectorRate, 0)

	// A sustained burst is one anomaly, not one per bucket.
	for b := 0; b < 3; b++ {
		for i := 0; i < 40; i++ {
			h.at(250 * time.Millisecond)
			h.read(fmt.Sprintf("/w/b%d_%d", b, i))
		}
	}
	h.wantIncidents(DetectorRate, 1)
	// Back to normal for a while, then a new burst is a new episode.
	steadyCalls(h, 5, 2)
	h.s.ClearIncidents()
	for i := 0; i < 40; i++ {
		h.at(100 * time.Millisecond)
		h.read(fmt.Sprintf("/w/again%d", i))
	}
	h.wantIncidents(DetectorRate, 1)
}

func TestRateTrendModel(t *testing.T) {
	h := newHarness(t, chain(only(DetectorRate), func(c *Config) { c.Rate.Trend = true }))
	steadyCalls(h, 30, 3)
	for i := 0; i < 50; i++ {
		h.at(100 * time.Millisecond)
		h.read(fmt.Sprintf("/w/burst%d", i))
	}
	h.wantIncidents(DetectorRate, 1)
}

func TestRateTokens(t *testing.T) {
	h := newHarness(t, only(DetectorRate))
	for i := 0; i < 30; i++ {
		h.at(10 * time.Second)
		h.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 20_000 + int64(i%3)*1000})
	}
	h.wantIncidents(DetectorRate, 0)
	h.at(10 * time.Second)
	h.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 400_000})
	if inc := h.wantIncidents(DetectorRate, 1)[0]; inc.Rule != "tokens" {
		t.Fatalf("rule %q", inc.Rule)
	}
}

// ---- burn

func TestBurnForecast(t *testing.T) {
	h := newHarness(t, only(DetectorBurn))
	h.s.SetBudget(h.sess, 1_000_000, 10)
	// 20k tokens every 10s = 120k/min: 1M lasts ~8 min → low (warn horizon 15m).
	for i := 0; i < 6; i++ {
		h.at(10 * time.Second)
		h.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 20_000, Cost: 0.01})
	}
	inc := h.wantIncidents(DetectorBurn, 1)[0]
	if inc.Rule != "tokens" || inc.Severity != SeverityLow {
		t.Fatalf("incident %+v", inc)
	}
	if r := inc.Metrics["rate_per_minute"]; math.Abs(r-120_000) > 1 {
		t.Fatalf("rate %v", r)
	}
	fc := h.s.Forecast(h.sess)
	if !fc[0].OK || fc[0].TimeToExhaustion < 6*time.Minute || fc[0].TimeToExhaustion > 8*time.Minute {
		t.Fatalf("forecast %+v", fc[0])
	}
	if fc[1].OK && fc[1].TimeToExhaustion < 15*time.Minute {
		t.Fatalf("cost forecast should be far: %+v", fc[1])
	}
	// Speeding up to 200k per 10s brings exhaustion within 3 minutes: high.
	for i := 0; i < 4; i++ {
		h.at(10 * time.Second)
		h.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 100_000})
	}
	got := h.incidents(DetectorBurn)
	if got[len(got)-1].Severity != SeverityHigh {
		t.Fatalf("no escalation: %+v", got)
	}
}

func TestBurnFalsePositives(t *testing.T) {
	h := newHarness(t, only(DetectorBurn))
	// No budget: no forecast.
	for i := 0; i < 10; i++ {
		h.at(10 * time.Second)
		h.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 100_000})
	}
	h.wantIncidents(DetectorBurn, 0)
	// Slow burn against a big budget, and too-short spans.
	h2 := newHarness(t, chain(only(DetectorBurn), func(c *Config) { c.Burn.TokenBudget = 10_000_000 }))
	for i := 0; i < 10; i++ {
		h2.at(30 * time.Second)
		h2.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 5_000})
	}
	for i := 0; i < 10; i++ {
		h2.at(time.Millisecond)
		h2.obs(Event{Kind: KindLLMResponse, Tool: "m", Tokens: 1})
	}
	h2.wantIncidents(DetectorBurn, 0)
}

// ---- deletion

func TestMassDeletion(t *testing.T) {
	h := newHarness(t, only(DetectorDeletion))
	for i := 0; i < 30; i++ {
		h.at(time.Second)
		h.obs(Event{Kind: KindFileDelete, Tool: "delete", Target: fmt.Sprintf("/w/f%d", i)})
	}
	inc := h.wantIncidents(DetectorDeletion, 1)[0]
	if inc.Severity != SeverityHigh {
		t.Fatalf("incident %+v", inc)
	}
	for i := 0; i < 10; i++ {
		h.exec("rm -rf src/*/generated/* docs/*")
	}
	got := h.wantIncidents(DetectorDeletion, 2)
	if got[1].Severity != SeverityCritical {
		t.Fatalf("no escalation: %+v", got[1])
	}
}

func TestDeletionFalsePositives(t *testing.T) {
	h := newHarness(t, only(DetectorDeletion))
	for i := 0; i < 200; i++ { // a steady trickle well under the rate
		h.at(10 * time.Second)
		h.exec(fmt.Sprintf("rm /tmp/scratch%d", i))
	}
	h.exec("ls -la && cat README.md")
	h.wantIncidents(DetectorDeletion, 0)
}

// ---- errors

func TestErrorStorm(t *testing.T) {
	h := newHarness(t, only(DetectorErrors))
	for i := 0; i < 10; i++ {
		h.at(2 * time.Second)
		h.result("bash", nil)
	}
	for i := 0; i < 12; i++ {
		h.at(2 * time.Second)
		h.result("bash", errBoom)
	}
	inc := h.wantIncidents(DetectorErrors, 1)[0]
	if inc.Metrics["ratio"] < 0.6 {
		t.Fatalf("incident %+v", inc)
	}
	// Recovery ends the storm; a new storm is a new episode.
	for i := 0; i < 40; i++ {
		h.at(2 * time.Second)
		h.result("bash", nil)
	}
	h.s.ClearIncidents()
	for i := 0; i < 8; i++ {
		h.at(time.Second)
		h.result("bash", errBoom)
	}
	h.wantIncidents(DetectorErrors, 1)
}

func TestErrorStormFalsePositives(t *testing.T) {
	h := newHarness(t, only(DetectorErrors))
	for i := 0; i < 300; i++ { // 20% failures, never more than 2 in a row
		h.at(2 * time.Second)
		var err error
		if i%5 == 0 {
			err = errBoom
		}
		h.result("bash", err)
	}
	for i := 0; i < 6; i++ { // a few scattered failures
		h.at(time.Minute)
		h.result("bash", errBoom)
	}
	h.wantIncidents(DetectorErrors, 0)
}

// ---- entropy

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestEntropyDetection(t *testing.T) {
	h := newHarness(t, only(DetectorEntropy))
	blob := []byte(base64.StdEncoding.EncodeToString(randomBytes(3000)))
	h.write("/w/internal/x/data.go", blob)
	h.write("/home/u/.cache/x/stage", randomBytes(4096))
	h.write("/w/hex.txt", []byte(fmt.Sprintf("%x", randomBytes(1024))))
	got := h.wantIncidents(DetectorEntropy, 3)
	want := []struct {
		rule string
		sev  Severity
	}{{"base64", SeverityLow}, {"binary", SeverityHigh}, {"hex", SeverityLow}}
	for i, w := range want {
		if got[i].Rule != w.rule || got[i].Severity != w.sev {
			t.Errorf("incident %d: %s/%s, want %s/%s", i, got[i].Rule, got[i].Severity, w.rule, w.sev)
		}
	}
}

func TestEntropyFalsePositives(t *testing.T) {
	h := newHarness(t, only(DetectorEntropy))
	src := []byte(strings.Repeat("func main() {\n\tfmt.Println(\"hello, world\", x[i]+y)\n}\n", 80))
	h.write("/w/main.go", src)
	h.write("/w/logo.png", randomBytes(8192))
	h.write("/w/go.sum", []byte(strings.Repeat("github.com/a/b v1.2.3 h1:"+base64.StdEncoding.EncodeToString(randomBytes(32))+"\n", 40)))
	h.write("/w/small.bin", randomBytes(100))
	// Wrapped PEM-style base64 with a header is still base64 but in a .go
	// path it is only a low finding; prose never is.
	prose := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog while the agent writes documentation. ", 40))
	h.write("/w/README.md", prose)
	h.wantIncidents(DetectorEntropy, 0)
}

func TestUnusualPath(t *testing.T) {
	cases := map[string]bool{
		"/w/internal/a.go":     false,
		"/tmp/x.go":            true,
		"/w/.hidden/x.go":      true,
		"/w/bin/payload":       true,
		"/w/.github/ci/x.yml":  false,
		"/w/../w/a.go":         false,
		"/home/u/.bashrc":      true,
		"c:/users/u/a.txt":     false,
		"/var/tmp/stage.dat":   true,
		"/w/node_modules/a.js": false,
	}
	for p, want := range cases {
		if got := unusualPath([]byte(p), 0); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
	if !unusualPath([]byte("/w/a.go"), FlagOutsideRoot) {
		t.Error("outside root not unusual")
	}
}
