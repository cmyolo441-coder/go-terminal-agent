package budget

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
)

const model = "claude-opus-5-5" // $4 in / $20 out per million tokens

// dollars returns usage on model costing exactly usd (input tokens only).
func dollars(usd float64) llm.Usage {
	return llm.Usage{InputTokens: int64(math.Round(usd / 4 * 1_000_000))}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func newTracker(t *testing.T, opts Options) *Tracker {
	t.Helper()
	tr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestNoLimitsAllows(t *testing.T) {
	tr := newTracker(t, Options{})
	tr.Record(model, dollars(1000))
	if d := tr.Check(model, 1e6, 1e6); d.Verdict != Allow {
		t.Fatalf("decision = %+v", d)
	}
}

func TestSessionLimit(t *testing.T) {
	tr := newTracker(t, Options{Limits: Limits{SessionUSD: 10}})
	if d := tr.Check(model, 0, 0); d.Verdict != Allow {
		t.Fatal(d)
	}
	res, err := tr.Record(model, dollars(7.5))
	if err != nil || !res.Priced || !near(res.CostUSD, 7.5) || len(res.Crossed) != 0 {
		t.Fatalf("res = %+v %v", res, err)
	}
	res, _ = tr.Record(model, dollars(1))
	if len(res.Crossed) != 1 || res.Crossed[0].Verdict != Warn || res.Crossed[0].Scope != ScopeSession {
		t.Fatalf("crossed = %+v", res.Crossed)
	}
	d := tr.Check(model, 0, 0)
	if d.Verdict != Warn || d.Scope != ScopeSession || !strings.Contains(d.Reason, "85% of the session budget") {
		t.Fatalf("decision = %+v", d)
	}
	// The next call could push past the cap: warn with the projected cost.
	d = tr.Check(model, 100_000, 64_000)
	if d.Verdict != Warn {
		t.Fatalf("decision = %+v", d)
	}
	res, _ = tr.Record(model, dollars(2))
	if len(res.Crossed) != 1 || res.Crossed[0].Verdict != Block {
		t.Fatalf("crossed = %+v", res.Crossed)
	}
	d = tr.Check(model, 0, 0)
	if d.Verdict != Block || !strings.Contains(d.Reason, "session budget of $10.00 reached ($10.50 spent)") || !near(d.SpentUSD, 10.5) || d.LimitUSD != 10 {
		t.Fatalf("decision = %+v", d)
	}
	// Further records do not report the same crossing again.
	if res, _ := tr.Record(model, dollars(1)); len(res.Crossed) != 0 {
		t.Fatalf("crossed again: %+v", res.Crossed)
	}
	if s := tr.Session(); s.Calls != 4 || !near(s.CostUSD, 11.5) {
		t.Fatalf("session = %+v", s)
	}
}

func TestProjectedCostWarns(t *testing.T) {
	tr := newTracker(t, Options{Limits: Limits{DailyUSD: 2, WarnAt: 0.99}})
	tr.Record(model, dollars(1))
	// 64k output tokens on opus-5-5 may cost $1.28 → past the $2 cap.
	d := tr.Check(model, 0, 64_000)
	if d.Verdict != Warn || d.Scope != ScopeDaily || !strings.Contains(d.Reason, "could cost up to $1.28") {
		t.Fatalf("decision = %+v", d)
	}
	if d := tr.Check(model, 0, 1000); d.Verdict != Allow {
		t.Fatalf("small call: %+v", d)
	}
}

func TestDailySharedAcrossTrackersAndMonthly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gta", LedgerFile)
	c := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	opts := Options{Path: path, Now: c.now, Location: time.UTC, Limits: Limits{DailyUSD: 5, MonthlyUSD: 12}}
	a := newTracker(t, opts)
	b := newTracker(t, opts)
	a.Record(model, dollars(3))
	b.Record("claude-haiku-5-5", llm.Usage{OutputTokens: 4_000_000}) // $2
	if d := b.Check(model, 0, 0); d.Verdict != Block || d.Scope != ScopeDaily {
		t.Fatalf("b sees a's spend: %+v", d)
	}
	if !near(a.Session().CostUSD, 3) || !near(b.Session().CostUSD, 2) {
		t.Fatal("session spend must stay per tracker")
	}
	// The next day the daily cap resets but the month keeps counting.
	c.set(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	if d := a.Check(model, 0, 0); d.Verdict != Allow {
		t.Fatalf("new day: %+v", d)
	}
	res, err := a.Record(model, dollars(5))
	if err != nil {
		t.Fatal(err)
	}
	if !near(res.Month.CostUSD, 10) || !near(res.Day.CostUSD, 5) || len(res.Crossed) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if d := a.Check(model, 0, 0); d.Verdict != Block || d.Scope != ScopeDaily {
		t.Fatalf("decision = %+v", d)
	}
	c.set(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	if d := a.Check(model, 0, 0); d.Verdict != Warn || d.Scope != ScopeMonthly {
		t.Fatalf("monthly warn: %+v", d)
	}
	// A new month starts from zero.
	c.set(time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC))
	if d := a.Check(model, 0, 0); d.Verdict != Allow {
		t.Fatalf("new month: %+v", d)
	}
	st, err := a.Status()
	if err != nil || st.Day != "2026-12-01" || st.Month != "2026-12" || st.ThisMonth.Calls != 0 {
		t.Fatalf("status = %+v %v", st, err)
	}
	c.set(time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC))
	st, _ = a.Status()
	if got := st.Models(); len(got) != 2 || got[0] != model || st.TodayByModel["claude-haiku-5-5"].OutputTokens != 4_000_000 {
		t.Fatalf("models = %v %+v", got, st.TodayByModel)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("ledger mode %v", fi.Mode())
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestDayBoundaryUsesLocation(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*3600)
	c := &clock{t: time.Date(2026, 11, 13, 16, 0, 0, 0, time.UTC)} // 01:00 on the 14th in UTC+9
	tr := newTracker(t, Options{Now: c.now, Location: loc})
	tr.Record(model, dollars(1))
	st, _ := tr.Status()
	if st.Day != "2026-11-14" || !near(st.Today.CostUSD, 1) {
		t.Fatalf("status = %+v", st)
	}
}

func TestUnpricedModels(t *testing.T) {
	nv := "mistralai/mistral-large-4"
	tr := newTracker(t, Options{Limits: Limits{SessionUSD: 1}})
	res, _ := tr.Record(nv, llm.Usage{InputTokens: 1e6})
	if res.Priced || res.CostUSD != 0 || res.Session.UnpricedCalls != 1 {
		t.Fatalf("res = %+v", res)
	}
	if d := tr.Check(nv, 0, 0); d.Verdict != Allow {
		t.Fatalf("without BlockUnpriced: %+v", d)
	}
	strict := newTracker(t, Options{Limits: Limits{SessionUSD: 1}, BlockUnpriced: true})
	if d := strict.Check(nv, 0, 0); d.Verdict != Block || !strings.Contains(d.Reason, "no known price") {
		t.Fatalf("BlockUnpriced: %+v", d)
	}
	if d := strict.Check(model, 0, 0); d.Verdict != Allow {
		t.Fatalf("priced model blocked: %+v", d)
	}
	fb := newTracker(t, Options{FallbackRate: &pricing.Rate{Input: 2, Output: 6}, BlockUnpriced: true, Limits: Limits{SessionUSD: 1}})
	res, _ = fb.Record(nv, llm.Usage{InputTokens: 1e6, OutputTokens: 1e6})
	if !res.Priced || !near(res.CostUSD, 8) {
		t.Fatalf("fallback: %+v", res)
	}
	if !fb.Priced(nv) || fb.Check(nv, 0, 0).Verdict != Block {
		t.Fatal("fallback-priced model should be enforced")
	}
}

func TestCorruptLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LedgerFile)
	os.WriteFile(path, []byte("{not json"), 0o600)

	tr := newTracker(t, Options{Path: path, Limits: Limits{DailyUSD: 5}})
	if d := tr.Check(model, 0, 0); d.Verdict != Warn || !strings.Contains(d.Reason, "unreadable") {
		t.Fatalf("decision = %+v", d)
	}
	res, err := tr.Record(model, dollars(1))
	if err != nil || !strings.Contains(res.Warning, "moved to") {
		t.Fatalf("record: %+v %v", res, err)
	}
	if d := tr.Check(model, 0, 0); d.Verdict != Allow {
		t.Fatalf("after recovery: %+v", d)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt copy: %v", matches)
	}

	os.WriteFile(path, []byte(`{"version":9,"days":{}}`), 0o600)
	if _, err := New(Options{Path: path, FailClosed: true}); err == nil {
		t.Fatal("FailClosed should refuse an unreadable ledger")
	}
	os.Remove(path)
	strict := newTracker(t, Options{Path: path, FailClosed: true, Limits: Limits{DailyUSD: 5}})
	os.WriteFile(path, []byte(`{"version":1,"days":{"yesterday":{}}}`), 0o600)
	if d := strict.Check(model, 0, 0); d.Verdict != Block {
		t.Fatalf("FailClosed check: %+v", d)
	}
	if _, err := strict.Record(model, dollars(1)); err == nil {
		t.Fatal("FailClosed record should fail")
	}
	if !near(strict.Session().CostUSD, 1) {
		t.Fatal("session spend must still be counted")
	}
}

func TestRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), LedgerFile)
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	tr := newTracker(t, Options{Path: path, Now: c.now, Location: time.UTC, RetainDays: 30})
	tr.Record(model, dollars(1))
	c.set(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	tr.Record(model, dollars(1))
	var l ledger
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &l)
	if len(l.Days) != 1 {
		t.Fatalf("days = %v", l.Days)
	}
}

func TestLimitsValidateAndTighten(t *testing.T) {
	for _, bad := range []Limits{{SessionUSD: -1}, {DailyUSD: math.NaN()}, {MonthlyUSD: math.Inf(1)}, {WarnAt: 1.5}} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be invalid", bad)
		}
		if _, err := New(Options{Limits: bad}); err == nil {
			t.Errorf("New(%+v) should fail", bad)
		}
	}
	user := Limits{SessionUSD: 20, DailyUSD: 0, MonthlyUSD: 300, WarnAt: 0.9}
	org := Limits{SessionUSD: 5, DailyUSD: 50, MonthlyUSD: 500}
	got := user.Tighten(org)
	if got != (Limits{SessionUSD: 5, DailyUSD: 50, MonthlyUSD: 300, WarnAt: 0.9}) {
		t.Fatalf("tighten = %+v", got)
	}
	tr := newTracker(t, Options{})
	if err := tr.SetLimits(Limits{SessionUSD: -2}); err == nil {
		t.Fatal("SetLimits accepted negative cap")
	}
	if err := tr.SetLimits(got); err != nil || tr.Limits() != got {
		t.Fatal("SetLimits")
	}
	for v, s := range map[Verdict]string{Allow: "allow", Warn: "warn", Block: "block"} {
		if v.String() != s {
			t.Fatal(v)
		}
	}
	if usd(0.0123) != "$0.0123" || usd(12.5) != "$12.50" {
		t.Fatal(usd(0.0123), usd(12.5))
	}
}

func TestHelperBudgetProcess(t *testing.T) {
	if os.Getenv("BUDGET_HELPER") != "1" {
		t.Skip("helper process only")
	}
	tr, err := New(Options{Path: os.Getenv("BUDGET_PATH")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, err := tr.Record(model, llm.Usage{InputTokens: 250_000, OutputTokens: 10}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentProcessesDoNotLoseSpend(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	path := filepath.Join(t.TempDir(), LedgerFile)
	const procs = 4
	var cmds []*exec.Cmd
	for i := 0; i < procs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperBudgetProcess$")
		cmd.Env = append(os.Environ(), "BUDGET_HELPER=1", "BUDGET_PATH="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	// And goroutines in this process, through separate trackers.
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		tr := newTracker(t, Options{Path: path})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := tr.Record(model, llm.Usage{InputTokens: 250_000, OutputTokens: 10}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	st, err := newTracker(t, Options{Path: path}).Status()
	if err != nil {
		t.Fatal(err)
	}
	want := int64((procs + 3) * 25)
	if st.Today.Calls != want || st.Today.OutputTokens != want*10 || !near(st.Today.CostUSD, float64(want)*(1.0+0.0002)) {
		t.Fatalf("today = %+v, want %d calls", st.Today, want)
	}
}
