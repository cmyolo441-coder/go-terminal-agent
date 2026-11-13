// Package budget enforces spending limits on model calls.
//
// A Tracker holds per-session spend in memory and daily and monthly spend
// in a small JSON ledger shared by every gta process of the user. The
// ledger is updated under a cross-process file lock and replaced
// atomically (write to a temporary file, fsync, rename), so concurrent
// sessions never lose each other's spend and a crash never leaves a
// half-written ledger.
//
// The agent calls Check before each model call (Allow, Warn or Block, with
// a reason) and Record after it, with the model and token usage; Record
// prices the usage with internal/pricing.
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/audit/filelock"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/pricing"
)

// DefaultWarnAt is the fraction of a limit at which Check starts warning.
const DefaultWarnAt = 0.8

// LedgerFile is the ledger's file name inside the config directory.
const LedgerFile = "budget.json"

// Limits are USD caps. Zero means no cap.
type Limits struct {
	SessionUSD float64 `json:"session_usd,omitempty"`
	DailyUSD   float64 `json:"daily_usd,omitempty"`
	MonthlyUSD float64 `json:"monthly_usd,omitempty"`
	// WarnAt is the fraction of a cap at which to warn, in (0, 1]
	// (default 0.8).
	WarnAt float64 `json:"warn_at,omitempty"`
}

// Validate checks the limits.
func (l Limits) Validate() error {
	for name, v := range map[string]float64{"session_usd": l.SessionUSD, "daily_usd": l.DailyUSD, "monthly_usd": l.MonthlyUSD} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("budget: %s must be a non-negative number", name)
		}
	}
	if l.WarnAt < 0 || l.WarnAt > 1 || math.IsNaN(l.WarnAt) {
		return errors.New("budget: warn_at must be between 0 and 1")
	}
	return nil
}

// Active reports whether any cap is set.
func (l Limits) Active() bool { return l.SessionUSD > 0 || l.DailyUSD > 0 || l.MonthlyUSD > 0 }

// Tighten returns the stricter of l and o for every cap (a zero cap is
// "no cap", so any positive cap is stricter), and the lower warn threshold.
func (l Limits) Tighten(o Limits) Limits {
	pick := func(a, b float64) float64 {
		switch {
		case a <= 0:
			return b
		case b <= 0:
			return a
		}
		return math.Min(a, b)
	}
	return Limits{
		SessionUSD: pick(l.SessionUSD, o.SessionUSD),
		DailyUSD:   pick(l.DailyUSD, o.DailyUSD),
		MonthlyUSD: pick(l.MonthlyUSD, o.MonthlyUSD),
		WarnAt:     pick(l.WarnAt, o.WarnAt),
	}
}

func (l Limits) warnAt() float64 {
	if l.WarnAt <= 0 {
		return DefaultWarnAt
	}
	return l.WarnAt
}

// Verdict is the outcome of a check.
type Verdict int

const (
	Allow Verdict = iota
	Warn
	Block
)

func (v Verdict) String() string {
	switch v {
	case Warn:
		return "warn"
	case Block:
		return "block"
	}
	return "allow"
}

// Scope names a budget period.
type Scope string

const (
	ScopeSession Scope = "session"
	ScopeDaily   Scope = "daily"
	ScopeMonthly Scope = "monthly"
)

// Decision explains a check.
type Decision struct {
	Verdict Verdict
	Reason  string
	// Scope, SpentUSD and LimitUSD describe the cap that decided, if any.
	Scope    Scope
	SpentUSD float64
	LimitUSD float64
}

// Totals accumulate usage and cost.
type Totals struct {
	CostUSD          float64 `json:"cost_usd"`
	Calls            int64   `json:"calls"`
	UnpricedCalls    int64   `json:"unpriced_calls,omitempty"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
}

func (t *Totals) add(u llm.Usage, cost float64, priced bool) {
	t.CostUSD += cost
	t.Calls++
	if !priced {
		t.UnpricedCalls++
	}
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CacheReadTokens += u.CacheReadTokens
	t.CacheWriteTokens += u.CacheWriteTokens
}

func (t *Totals) merge(o Totals) {
	t.CostUSD += o.CostUSD
	t.Calls += o.Calls
	t.UnpricedCalls += o.UnpricedCalls
	t.InputTokens += o.InputTokens
	t.OutputTokens += o.OutputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
}

// Options configures a Tracker.
type Options struct {
	Limits Limits
	// Path is the ledger file, normally filepath.Join(config.UserDir(),
	// LedgerFile). Empty keeps daily and monthly spend in memory only.
	Path string
	// Location sets where days and months begin (default time.Local).
	Location *time.Location
	// Now is the clock (default time.Now).
	Now func() time.Time
	// FallbackRate prices models internal/pricing does not know. Without
	// it such calls cost $0 and are counted as unpriced.
	FallbackRate *pricing.Rate
	// BlockUnpriced makes Check block models that cannot be priced while
	// a cap is set, so a cap cannot be bypassed by picking such a model.
	BlockUnpriced bool
	// FailClosed blocks model calls when the ledger cannot be read, instead
	// of warning and continuing.
	FailClosed bool
	// RetainDays bounds the ledger's history (default 400).
	RetainDays int
	// LockTimeout bounds the wait for the ledger lock (default 10s).
	LockTimeout time.Duration
}

// Tracker enforces Limits. It is safe for concurrent use.
type Tracker struct {
	opts Options

	mu      sync.Mutex
	limits  Limits
	session Totals
	// mem is the ledger when Options.Path is empty.
	mem *ledger
}

// New validates opts and returns a Tracker. A ledger that exists but cannot
// be read is an error when FailClosed is set.
func New(opts Options) (*Tracker, error) {
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetainDays <= 0 {
		opts.RetainDays = 400
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 10 * time.Second
	}
	t := &Tracker{opts: opts, limits: opts.Limits}
	if opts.Path == "" {
		t.mem = newLedger()
	} else if _, err := t.load(); err != nil && opts.FailClosed {
		return nil, err
	}
	return t, nil
}

// Limits returns the current caps.
func (t *Tracker) Limits() Limits {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.limits
}

// SetLimits replaces the caps, for example from a /budget command. Callers
// that enforce a managed policy should Tighten the new limits with the
// policy's first.
func (t *Tracker) SetLimits(l Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	t.mu.Lock()
	t.limits = l
	t.mu.Unlock()
	return nil
}

// cost prices usage on model.
func (t *Tracker) cost(model string, u llm.Usage) (float64, bool) {
	if c, ok := pricing.Cost(model, u); ok {
		return c, true
	}
	if r := t.opts.FallbackRate; r != nil {
		const m = 1_000_000
		return float64(u.InputTokens)*r.Input/m + float64(u.OutputTokens)*r.Output/m +
			float64(u.CacheReadTokens)*r.CacheRead/m + float64(u.CacheWriteTokens)*r.CacheWrite/m, true
	}
	return 0, false
}

// Priced reports whether calls on model can be priced.
func (t *Tracker) Priced(model string) bool {
	_, ok := t.cost(model, llm.Usage{InputTokens: 1})
	return ok
}

// Check decides whether a model call may start. model, promptTokens and
// maxOutputTokens are optional; when given, Check also warns if the call's
// worst-case cost would cross a cap.
func (t *Tracker) Check(model string, promptTokens, maxOutputTokens int64) Decision {
	t.mu.Lock()
	limits, session := t.limits, t.session.CostUSD
	t.mu.Unlock()
	if !limits.Active() {
		return Decision{Verdict: Allow}
	}
	if model != "" && t.opts.BlockUnpriced && !t.Priced(model) {
		return Decision{Verdict: Block, Reason: fmt.Sprintf("model %q has no known price, so the spending cap cannot be enforced; pick a priced model", model)}
	}
	day, month, err := t.periodSpend()
	if err != nil {
		if t.opts.FailClosed {
			return Decision{Verdict: Block, Reason: "budget ledger unreadable: " + err.Error()}
		}
		return Decision{Verdict: Warn, Reason: "budget ledger unreadable, daily and monthly caps not enforced: " + err.Error()}
	}
	var worst float64
	if model != "" && (promptTokens > 0 || maxOutputTokens > 0) {
		worst, _ = t.cost(model, llm.Usage{InputTokens: promptTokens, OutputTokens: maxOutputTokens})
	}
	best := Decision{Verdict: Allow}
	bestFrac := -1.0
	consider := func(scope Scope, spent, limit float64) {
		if limit <= 0 {
			return
		}
		d := Decision{Scope: scope, SpentUSD: spent, LimitUSD: limit}
		frac := spent / limit
		switch {
		case spent >= limit:
			d.Verdict = Block
			d.Reason = fmt.Sprintf("%s budget of %s reached (%s spent)", scope, usd(limit), usd(spent))
		case frac >= limits.warnAt():
			d.Verdict = Warn
			d.Reason = fmt.Sprintf("%.0f%% of the %s budget used (%s of %s)", frac*100, scope, usd(spent), usd(limit))
		case worst > 0 && spent+worst > limit:
			d.Verdict = Warn
			d.Reason = fmt.Sprintf("this call could cost up to %s and pass the %s budget (%s of %s used)", usd(worst), scope, usd(spent), usd(limit))
		default:
			return
		}
		if d.Verdict > best.Verdict || (d.Verdict == best.Verdict && frac > bestFrac) {
			best, bestFrac = d, frac
		}
	}
	consider(ScopeSession, session, limits.SessionUSD)
	consider(ScopeDaily, day.CostUSD, limits.DailyUSD)
	consider(ScopeMonthly, month.CostUSD, limits.MonthlyUSD)
	return best
}

// Result reports what a Record call did.
type Result struct {
	CostUSD float64
	// Priced is false when the model has no known price.
	Priced bool
	// Session, Day and Month are the totals after this call.
	Session, Day, Month Totals
	// Crossed lists the warn thresholds and caps this call crossed.
	Crossed []Decision
	// Warning reports a ledger problem that did not stop recording.
	Warning string
}

// Record adds a finished call's usage to the session, the day and the month.
// The session total is updated even when the ledger cannot be written.
func (t *Tracker) Record(model string, u llm.Usage) (Result, error) {
	cost, priced := t.cost(model, u)
	res := Result{CostUSD: cost, Priced: priced}
	t.mu.Lock()
	before := t.session
	t.session.add(u, cost, priced)
	res.Session = t.session
	limits := t.limits
	t.mu.Unlock()

	now := t.opts.Now().In(t.opts.Location)
	dayKey, monthKey := now.Format(time.DateOnly), now.Format("2006-01")
	var dayBefore, monthBefore Totals
	err := t.update(func(l *ledger) {
		d := l.Days[dayKey]
		dayBefore = d.Totals
		monthBefore = l.month(monthKey)
		d.Totals.add(u, cost, priced)
		if d.Models == nil {
			d.Models = map[string]Totals{}
		}
		mt := d.Models[model]
		mt.add(u, cost, priced)
		d.Models[model] = mt
		l.Days[dayKey] = d
		l.prune(now, t.opts.RetainDays)
		res.Day = d.Totals
		res.Month = l.month(monthKey)
	}, &res)
	crossed := func(scope Scope, from, to, limit float64) {
		if limit <= 0 {
			return
		}
		if from < limit && to >= limit {
			res.Crossed = append(res.Crossed, Decision{Verdict: Block, Scope: scope, SpentUSD: to, LimitUSD: limit,
				Reason: fmt.Sprintf("%s budget of %s reached (%s spent); further model calls are blocked", scope, usd(limit), usd(to))})
		} else if w := limit * limits.warnAt(); from < w && to >= w {
			res.Crossed = append(res.Crossed, Decision{Verdict: Warn, Scope: scope, SpentUSD: to, LimitUSD: limit,
				Reason: fmt.Sprintf("%.0f%% of the %s budget used (%s of %s)", to/limit*100, scope, usd(to), usd(limit))})
		}
	}
	crossed(ScopeSession, before.CostUSD, res.Session.CostUSD, limits.SessionUSD)
	if err == nil {
		crossed(ScopeDaily, dayBefore.CostUSD, res.Day.CostUSD, limits.DailyUSD)
		crossed(ScopeMonthly, monthBefore.CostUSD, res.Month.CostUSD, limits.MonthlyUSD)
	}
	return res, err
}

// Status is a snapshot for display.
type Status struct {
	Limits    Limits
	Day       string
	Month     string
	Session   Totals
	Today     Totals
	ThisMonth Totals
	// TodayByModel breaks today's spend down by model.
	TodayByModel map[string]Totals
}

// Status reads the current spend.
func (t *Tracker) Status() (Status, error) {
	t.mu.Lock()
	st := Status{Limits: t.limits, Session: t.session}
	t.mu.Unlock()
	now := t.opts.Now().In(t.opts.Location)
	st.Day, st.Month = now.Format(time.DateOnly), now.Format("2006-01")
	l, err := t.load()
	if err != nil {
		return st, err
	}
	d := l.Days[st.Day]
	st.Today = d.Totals
	st.TodayByModel = map[string]Totals{}
	for m, v := range d.Models {
		st.TodayByModel[m] = v
	}
	st.ThisMonth = l.month(st.Month)
	return st, nil
}

// Session returns this session's totals.
func (t *Tracker) Session() Totals {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session
}

func (t *Tracker) periodSpend() (Totals, Totals, error) {
	l, err := t.load()
	if err != nil {
		return Totals{}, Totals{}, err
	}
	now := t.opts.Now().In(t.opts.Location)
	return l.Days[now.Format(time.DateOnly)].Totals, l.month(now.Format("2006-01")), nil
}

// ledger is the persisted state.
type ledger struct {
	Version int                 `json:"version"`
	Days    map[string]dayEntry `json:"days"`
}

type dayEntry struct {
	Totals
	Models map[string]Totals `json:"models,omitempty"`
}

const ledgerVersion = 1

func newLedger() *ledger { return &ledger{Version: ledgerVersion, Days: map[string]dayEntry{}} }

func (l *ledger) month(key string) Totals {
	var t Totals
	for day, d := range l.Days {
		if strings.HasPrefix(day, key+"-") {
			t.merge(d.Totals)
		}
	}
	return t
}

func (l *ledger) prune(now time.Time, keep int) {
	cutoff := now.AddDate(0, 0, -keep).Format(time.DateOnly)
	for day := range l.Days {
		if day < cutoff {
			delete(l.Days, day)
		}
	}
}

func (l *ledger) clone() *ledger {
	c := newLedger()
	for k, d := range l.Days {
		nd := dayEntry{Totals: d.Totals, Models: map[string]Totals{}}
		for m, v := range d.Models {
			nd.Models[m] = v
		}
		c.Days[k] = nd
	}
	return c
}

func decodeLedger(data []byte) (*ledger, error) {
	l := newLedger()
	if len(strings.TrimSpace(string(data))) == 0 {
		return l, nil
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, err
	}
	if l.Version != ledgerVersion {
		return nil, fmt.Errorf("unsupported ledger version %d", l.Version)
	}
	if l.Days == nil {
		l.Days = map[string]dayEntry{}
	}
	for day, d := range l.Days {
		if _, err := time.Parse(time.DateOnly, day); err != nil {
			return nil, fmt.Errorf("bad day %q", day)
		}
		if d.CostUSD < 0 || math.IsNaN(d.CostUSD) || math.IsInf(d.CostUSD, 0) {
			return nil, fmt.Errorf("bad cost on %s", day)
		}
	}
	return l, nil
}

// load reads the ledger. The file is only ever replaced by rename, so no
// lock is needed to read a consistent copy.
func (t *Tracker) load() (*ledger, error) {
	if t.opts.Path == "" {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.mem.clone(), nil
	}
	data, err := os.ReadFile(t.opts.Path)
	if errors.Is(err, os.ErrNotExist) {
		return newLedger(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	l, err := decodeLedger(data)
	if err != nil {
		return nil, fmt.Errorf("budget: %s: %w", t.opts.Path, err)
	}
	return l, nil
}

// update applies f to the ledger under the cross-process lock and saves it.
func (t *Tracker) update(f func(*ledger), res *Result) error {
	if t.opts.Path == "" {
		t.mu.Lock()
		defer t.mu.Unlock()
		f(t.mem)
		return nil
	}
	lock, err := filelock.AcquireTimeout(t.opts.Path+".lock", t.opts.LockTimeout)
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	defer lock.Unlock()
	l, err := t.load()
	if err != nil {
		if t.opts.FailClosed {
			return err
		}
		// Keep the damaged file for inspection and start a new ledger, so
		// spend keeps being counted from here on.
		aside := fmt.Sprintf("%s.corrupt-%d", t.opts.Path, time.Now().UnixNano())
		if rerr := os.Rename(t.opts.Path, aside); rerr != nil {
			return errors.Join(err, rerr)
		}
		res.Warning = fmt.Sprintf("budget ledger was unreadable and was moved to %s; daily and monthly totals restart now", filepath.Base(aside))
		l = newLedger()
	}
	f(l)
	return writeAtomic(t.opts.Path, l)
}

func writeAtomic(path string, l *ledger) error {
	data, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".budget-*.tmp")
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("budget: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	ok = true
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}

// usd formats a dollar amount with cents, or more precision below $1.
func usd(v float64) string {
	if v != 0 && math.Abs(v) < 1 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// Models lists today's models by spend, highest first, for display.
func (s Status) Models() []string {
	out := make([]string, 0, len(s.TodayByModel))
	for m := range s.TodayByModel {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := s.TodayByModel[out[i]].CostUSD, s.TodayByModel[out[j]].CostUSD
		if a != b {
			return a > b
		}
		return out[i] < out[j]
	})
	return out
}
