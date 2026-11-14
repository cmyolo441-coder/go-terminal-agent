package memory

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind classifies a memory.
type Kind string

// Memory kinds.
const (
	// KindFact is a piece of knowledge about the world, the user or the
	// codebase ("the API server listens on :8443").
	KindFact Kind = "fact"
	// KindPreference is how the user likes things done ("prefers
	// table-driven tests").
	KindPreference Kind = "preference"
	// KindDecision is a choice that was made and should be respected later
	// ("we use pgx, not database/sql").
	KindDecision Kind = "decision"
	// KindEpisode is something that happened ("migrating to Go 1.26 broke
	// the race tests on CI").
	KindEpisode Kind = "episode"
)

// Kinds lists every valid kind.
var Kinds = []Kind{KindFact, KindPreference, KindDecision, KindEpisode}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return kindCode(k) != 0 }

func kindCode(k Kind) uint8 {
	switch k {
	case KindFact:
		return 1
	case KindPreference:
		return 2
	case KindDecision:
		return 3
	case KindEpisode:
		return 4
	}
	return 0
}

func kindFromCode(c uint8) (Kind, bool) {
	if c == 0 || int(c) > len(Kinds) {
		return "", false
	}
	return Kinds[c-1], true
}

// Scope says who a memory belongs to.
type Scope string

// Memory scopes.
const (
	// ScopeUser memories follow the user everywhere.
	ScopeUser Scope = "user"
	// ScopeProject memories belong to one project, named by the record's
	// Namespace (typically the repository root or a stable project key).
	ScopeProject Scope = "project"
	// ScopeSession memories belong to one session, named by Namespace.
	ScopeSession Scope = "session"
)

// Scopes lists every valid scope.
var Scopes = []Scope{ScopeUser, ScopeProject, ScopeSession}

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return scopeCode(s) != 0 }

func scopeCode(s Scope) uint8 {
	switch s {
	case ScopeUser:
		return 1
	case ScopeProject:
		return 2
	case ScopeSession:
		return 3
	}
	return 0
}

func scopeFromCode(c uint8) (Scope, bool) {
	if c == 0 || int(c) > len(Scopes) {
		return "", false
	}
	return Scopes[c-1], true
}

// Record is one memory.
type Record struct {
	// ID identifies the record. Remember assigns a new, time-ordered ID when
	// it is empty; remembering a record with an existing ID replaces it.
	ID   string
	Kind Kind
	// Text is the memory itself, one self-contained statement.
	Text string
	// Tags are short labels; they are matched lexically with a boost.
	Tags  []string
	Scope Scope
	// Namespace names the project (ScopeProject) or session (ScopeSession)
	// the record belongs to. It must be empty for ScopeUser.
	Namespace string
	// Created is when the memory was first recorded; Updated when it last
	// changed; LastAccess when it was last created, changed or recalled.
	Created    time.Time
	Updated    time.Time
	LastAccess time.Time
	// Importance in (0, 1]. Zero selects the default for the kind. A record
	// with importance 1 is pinned: the size budget never evicts it.
	Importance float64
	// TTL, when positive, expires the record TTL after Updated: replacing
	// or reinforcing a memory (a near-duplicate merged into it) renews it.
	TTL time.Duration
	// Source says where the memory came from ("user", "agent",
	// "consolidation", ...). Free-form.
	Source string
	// AccessCount counts recalls that returned this record.
	AccessCount uint32
	// Reinforced counts how often a near-duplicate was merged into it.
	Reinforced uint32
	// MergedFrom lists the IDs a consolidation summarised (capped).
	MergedFrom []string
}

// Expired reports whether the record's TTL has elapsed at now.
func (r *Record) Expired(now time.Time) bool {
	return r.TTL > 0 && !r.Updated.IsZero() && now.Sub(r.Updated) >= r.TTL
}

// Pinned reports whether the record is exempt from budget eviction.
func (r *Record) Pinned() bool { return r.Importance >= 1 }

// Clone returns a deep copy.
func (r Record) Clone() Record {
	r.Tags = slices.Clone(r.Tags)
	r.MergedFrom = slices.Clone(r.MergedFrom)
	return r
}

// Limits on record contents.
const (
	maxTags        = 32
	maxTagBytes    = 64
	maxNamespace   = 1024
	maxSourceBytes = 64
	maxMergedFrom  = 64
	maxIDBytes     = 128
)

// Errors returned by the store.
var (
	ErrInvalid      = errors.New("memory: invalid record")
	ErrNotFound     = errors.New("memory: record not found")
	ErrClosed       = errors.New("memory: store closed")
	ErrCorrupt      = errors.New("memory: corrupt data")
	ErrNoSummarizer = errors.New("memory: no summarizer configured")
	ErrEmbedding    = errors.New("memory: embedding failed")
)

func defaultImportance(k Kind) float64 {
	switch k {
	case KindPreference, KindDecision:
		return 0.7
	case KindFact:
		return 0.5
	default:
		return 0.3
	}
}

// normalizeRecord redacts and validates r in place, filling defaults.
func normalizeRecord(r *Record, redact func(string) string, maxText int, now time.Time) error {
	if r.Kind == "" {
		r.Kind = KindFact
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, r.Kind)
	}
	if r.Scope == "" {
		r.Scope = ScopeUser
	}
	if !r.Scope.Valid() {
		return fmt.Errorf("%w: unknown scope %q", ErrInvalid, r.Scope)
	}
	r.Text = cleanText(redact(r.Text))
	if r.Text == "" {
		return fmt.Errorf("%w: empty text", ErrInvalid)
	}
	if len(r.Text) > maxText {
		return fmt.Errorf("%w: text is %d bytes, limit %d", ErrInvalid, len(r.Text), maxText)
	}
	r.Namespace = strings.TrimSpace(redact(r.Namespace))
	switch {
	case r.Scope == ScopeUser && r.Namespace != "":
		return fmt.Errorf("%w: user-scoped records take no namespace", ErrInvalid)
	case r.Scope != ScopeUser && r.Namespace == "":
		return fmt.Errorf("%w: %s-scoped records need a namespace", ErrInvalid, r.Scope)
	case len(r.Namespace) > maxNamespace:
		return fmt.Errorf("%w: namespace longer than %d bytes", ErrInvalid, maxNamespace)
	}
	tags := make([]string, 0, len(r.Tags))
	for _, t := range r.Tags {
		t = normalizeTag(redact(t))
		if t != "" && !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if len(tags) > maxTags {
		return fmt.Errorf("%w: more than %d tags", ErrInvalid, maxTags)
	}
	r.Tags = tags
	r.Source = truncateUTF8(strings.TrimSpace(redact(r.Source)), maxSourceBytes)
	if r.ID != "" {
		if len(r.ID) > maxIDBytes || !utf8.ValidString(r.ID) || strings.ContainsFunc(r.ID, func(c rune) bool { return c <= ' ' }) {
			return fmt.Errorf("%w: bad id %q", ErrInvalid, r.ID)
		}
	} else {
		r.ID = NewID(now)
	}
	switch {
	case r.Importance == 0:
		r.Importance = defaultImportance(r.Kind)
	case r.Importance < 0 || r.Importance > 1 || r.Importance != r.Importance:
		return fmt.Errorf("%w: importance %v outside (0, 1]", ErrInvalid, r.Importance)
	}
	if r.TTL < 0 {
		return fmt.Errorf("%w: negative ttl", ErrInvalid)
	}
	if r.Created.IsZero() {
		r.Created = now
	}
	if r.Updated.IsZero() || r.Updated.Before(r.Created) {
		r.Updated = now
	}
	if r.LastAccess.IsZero() || r.LastAccess.Before(r.Updated) {
		r.LastAccess = r.Updated
	}
	if len(r.MergedFrom) > maxMergedFrom {
		r.MergedFrom = r.MergedFrom[len(r.MergedFrom)-maxMergedFrom:]
	}
	r.Created, r.Updated, r.LastAccess = r.Created.UTC(), r.Updated.UTC(), r.LastAccess.UTC()
	return nil
}

// cleanText trims, makes the text valid UTF-8, drops control characters
// other than newline and tab and collapses runs of blank lines.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	newlines := 0
	for _, c := range strings.TrimSpace(s) {
		switch {
		case c == '\r':
			continue
		case c == '\n':
			newlines++
			if newlines > 2 {
				continue
			}
		case c == '\t':
			newlines = 0
		case c < ' ' || c == 0x7f || (c >= 0x80 && c < 0xa0):
			continue
		default:
			newlines = 0
		}
		b.WriteRune(c)
	}
	return b.String()
}

func normalizeTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	t = strings.Join(strings.Fields(t), "-")
	return truncateUTF8(t, maxTagBytes)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a 26-character, lexically time-ordered identifier (48 bits
// of milliseconds followed by 80 random bits, Crockford base32, as in ULID)
// prefixed with "m".
func NewID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	_, _ = rand.Read(b[6:])
	var out [27]byte
	out[0] = 'm'
	// 128 bits into 26 base32 digits: the first digit carries 3 bits.
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 26; i >= 1; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// Filter restricts which records Recall and ForgetWhere consider. The zero
// value matches everything.
type Filter struct {
	// Kinds and Scopes, when non-empty, list the accepted values.
	Kinds  []Kind
	Scopes []Scope
	// Project and Session, when set, restrict project- and session-scoped
	// records to that namespace. User-scoped records are not affected.
	Project string
	Session string
	// Tags, when non-empty, requires at least one of these tags.
	Tags []string
	// MinImportance drops records below this importance.
	MinImportance float64
	// Since drops records last updated before this time.
	Since time.Time
	// IncludeExpired also returns records whose TTL has elapsed but which
	// have not been pruned yet.
	IncludeExpired bool
}

// compiledFilter is a Filter prepared for fast matching.
type compiledFilter struct {
	kinds, scopes    uint8 // bitsets by code; 0 = all
	project, session string
	tags             []string
	minImportance    float64
	since            time.Time
	includeExpired   bool
	now              time.Time
}

func (f Filter) compile(now time.Time) compiledFilter {
	c := compiledFilter{
		project: f.Project, session: f.Session,
		minImportance: f.MinImportance, since: f.Since,
		includeExpired: f.IncludeExpired, now: now,
	}
	for _, k := range f.Kinds {
		c.kinds |= 1 << kindCode(k)
	}
	for _, s := range f.Scopes {
		c.scopes |= 1 << scopeCode(s)
	}
	for _, t := range f.Tags {
		if t = normalizeTag(t); t != "" {
			c.tags = append(c.tags, t)
		}
	}
	return c
}

func (c *compiledFilter) match(r *Record) bool {
	if c.kinds != 0 && c.kinds&(1<<kindCode(r.Kind)) == 0 {
		return false
	}
	if c.scopes != 0 && c.scopes&(1<<scopeCode(r.Scope)) == 0 {
		return false
	}
	switch r.Scope {
	case ScopeProject:
		if c.project != "" && r.Namespace != c.project {
			return false
		}
	case ScopeSession:
		if c.session != "" && r.Namespace != c.session {
			return false
		}
	}
	if r.Importance < c.minImportance {
		return false
	}
	if !c.since.IsZero() && r.Updated.Before(c.since) {
		return false
	}
	if !c.includeExpired && r.Expired(c.now) {
		return false
	}
	if len(c.tags) > 0 && !slices.ContainsFunc(c.tags, func(t string) bool { return slices.Contains(r.Tags, t) }) {
		return false
	}
	return true
}
