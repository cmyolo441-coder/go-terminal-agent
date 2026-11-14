package supplychain

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OSVVulnerability is an advisory in the OSV schema (subset).
type OSVVulnerability struct {
	SchemaVersion    string          `json:"schema_version,omitempty"`
	ID               string          `json:"id"`
	Modified         string          `json:"modified,omitempty"`
	Published        string          `json:"published,omitempty"`
	Withdrawn        string          `json:"withdrawn,omitempty"`
	Aliases          []string        `json:"aliases,omitempty"`
	Related          []string        `json:"related,omitempty"`
	Summary          string          `json:"summary,omitempty"`
	Details          string          `json:"details,omitempty"`
	Severity         []OSVSeverity   `json:"severity,omitempty"`
	Affected         []OSVAffected   `json:"affected,omitempty"`
	References       []OSVReference  `json:"references,omitempty"`
	DatabaseSpecific json.RawMessage `json:"database_specific,omitempty"`
}

// OSVSeverity is a severity score entry.
type OSVSeverity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

// OSVPackage identifies an affected package.
type OSVPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	PURL      string `json:"purl,omitempty"`
}

// OSVAffected describes affected versions of one package.
type OSVAffected struct {
	Package           OSVPackage      `json:"package"`
	Severity          []OSVSeverity   `json:"severity,omitempty"`
	Ranges            []OSVRange      `json:"ranges,omitempty"`
	Versions          []string        `json:"versions,omitempty"`
	EcosystemSpecific json.RawMessage `json:"ecosystem_specific,omitempty"`
	DatabaseSpecific  json.RawMessage `json:"database_specific,omitempty"`
}

// OSVRange is an affected range expressed as ordered events.
type OSVRange struct {
	Type   string     `json:"type"`
	Repo   string     `json:"repo,omitempty"`
	Events []OSVEvent `json:"events"`
}

// OSVEvent is one range event; exactly one field is set.
type OSVEvent struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}

// OSVReference is an advisory link.
type OSVReference struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func (e OSVEvent) version() string {
	switch {
	case e.Introduced != "":
		return e.Introduced
	case e.Fixed != "":
		return e.Fixed
	case e.LastAffected != "":
		return e.LastAffected
	}
	return e.Limit
}

// osvEcosystemMatches compares an OSV ecosystem (which may carry a
// ":suffix" such as "Debian:12") to ours.
func osvEcosystemMatches(osv string, eco Ecosystem) bool {
	base, _, _ := strings.Cut(osv, ":")
	return strings.EqualFold(base, string(eco))
}

func osvNameMatches(eco Ecosystem, osvName, name string) bool {
	switch eco {
	case EcosystemPyPI:
		return NormalizePyPIName(osvName) == NormalizePyPIName(name)
	case EcosystemPackagist:
		return strings.EqualFold(osvName, name)
	}
	return osvName == name
}

// normalizeQueryVersion adapts a lockfile version to OSV conventions.
func normalizeQueryVersion(eco Ecosystem, v string) string {
	switch eco {
	case EcosystemPackagist:
		if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
			return v[1:]
		}
	}
	return v
}

// AffectedEntry returns the first affected entry that matches the package
// version, or nil. Withdrawn advisories never match.
func (v *OSVVulnerability) AffectedEntry(eco Ecosystem, name, version string) (*OSVAffected, error) {
	if v.Withdrawn != "" {
		return nil, nil
	}
	var firstErr error
	for i := range v.Affected {
		a := &v.Affected[i]
		if !osvEcosystemMatches(a.Package.Ecosystem, eco) || !osvNameMatches(eco, a.Package.Name, name) {
			continue
		}
		ok, err := a.Affects(eco, version)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ok {
			return a, nil
		}
	}
	return nil, firstErr
}

// Affects reports whether version is affected according to the explicit
// versions list or any SEMVER/ECOSYSTEM range, evaluated with the
// ecosystem's comparator using the OSV range-evaluation algorithm.
func (a *OSVAffected) Affects(eco Ecosystem, version string) (bool, error) {
	version = normalizeQueryVersion(eco, version)
	cmp := ComparatorFor(eco)
	for _, v := range a.Versions {
		if v == version {
			return true, nil
		}
		if c, err := cmp.Compare(v, version); err == nil && c == 0 {
			return true, nil
		}
	}
	var firstErr error
	for _, r := range a.Ranges {
		var rc VersionComparator
		switch r.Type {
		case "SEMVER":
			if eco == EcosystemGo {
				rc = cmp
			} else {
				rc = comparatorFunc(compareSemVerStrings)
			}
		case "ECOSYSTEM":
			rc = cmp
		default:
			continue // GIT ranges are resolved through the versions list
		}
		ok, err := rangeAffects(rc, r.Events, version)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, firstErr
}

func rangeAffects(cmp VersionComparator, events []OSVEvent, version string) (bool, error) {
	evs := append([]OSVEvent(nil), events...)
	var sortErr error
	less := func(a, b OSVEvent) int {
		av, bv := a.version(), b.version()
		if a.Introduced == "0" && b.Introduced == "0" {
			return 0
		}
		if a.Introduced == "0" {
			return -1
		}
		if b.Introduced == "0" {
			return 1
		}
		c, err := cmp.Compare(av, bv)
		if err != nil && sortErr == nil {
			sortErr = err
		}
		return c
	}
	sort.SliceStable(evs, func(i, j int) bool { return less(evs[i], evs[j]) < 0 })
	if sortErr != nil {
		return false, sortErr
	}
	ge := func(x string) (bool, error) {
		c, err := cmp.Compare(version, x)
		return c >= 0, err
	}
	affected := false
	for _, e := range evs {
		switch {
		case e.Introduced != "":
			if e.Introduced == "0" {
				affected = true
				continue
			}
			ok, err := ge(e.Introduced)
			if err != nil {
				return false, err
			}
			if ok {
				affected = true
			}
		case e.Fixed != "":
			ok, err := ge(e.Fixed)
			if err != nil {
				return false, err
			}
			if ok {
				affected = false
			}
		case e.LastAffected != "":
			c, err := cmp.Compare(version, e.LastAffected)
			if err != nil {
				return false, err
			}
			if c > 0 {
				affected = false
			}
		case e.Limit != "":
			if e.Limit == "*" {
				continue
			}
			ok, err := ge(e.Limit)
			if err != nil {
				return false, err
			}
			if ok {
				return false, nil
			}
		}
	}
	return affected, nil
}

// FixedVersions lists "fixed" events of the entry, sorted and de-duplicated.
func (a *OSVAffected) FixedVersions() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range a.Ranges {
		if r.Type == "GIT" {
			continue
		}
		for _, e := range r.Events {
			if e.Fixed != "" && !seen[e.Fixed] {
				seen[e.Fixed] = true
				out = append(out, e.Fixed)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---- Sources ---------------------------------------------------------------

// PackageQuery identifies one package version to check.
type PackageQuery struct {
	Ecosystem Ecosystem
	Name      string
	Version   string
}

// VulnSource returns, for each query, the advisories affecting it.
type VulnSource interface {
	Query(ctx context.Context, qs []PackageQuery) ([][]*OSVVulnerability, error)
}

// ---- Disk cache ------------------------------------------------------------

// DiskCache is a small content cache with a time-to-live. Entries are
// written atomically; expired entries are ignored on read.
type DiskCache struct {
	Dir string
	TTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

type cacheEnvelope struct {
	Stored time.Time       `json:"stored"`
	Data   json.RawMessage `json:"data"`
}

func (c *DiskCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *DiskCache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(c.Dir, h[:2], h+".json")
}

// Get returns cached JSON for key if present and fresh.
func (c *DiskCache) Get(key string) (json.RawMessage, bool) {
	if c == nil || c.Dir == "" {
		return nil, false
	}
	b, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	var env cacheEnvelope
	if json.Unmarshal(b, &env) != nil {
		return nil, false
	}
	if c.TTL > 0 && c.now().Sub(env.Stored) > c.TTL {
		return nil, false
	}
	return env.Data, true
}

// Put stores JSON data for key.
func (c *DiskCache) Put(key string, data json.RawMessage) error {
	if c == nil || c.Dir == "" {
		return nil
	}
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cacheEnvelope{Stored: c.now().UTC(), Data: data})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ---- Online client -----------------------------------------------------------

// DefaultOSVBaseURL is the public OSV API.
const DefaultOSVBaseURL = "https://api.osv.dev"

// OSVClient queries the OSV API: POST /v1/querybatch to find advisory IDs,
// then GET /v1/vulns/{id} for details. Responses are cached on disk.
type OSVClient struct {
	BaseURL    string
	HTTPClient *http.Client
	Cache      *DiskCache
	UserAgent  string
	// BatchSize bounds queries per querybatch request (max 1000).
	BatchSize int
	// Concurrency bounds parallel advisory fetches (default 8).
	Concurrency int
	// MaxRetries for 429/5xx responses (default 3).
	MaxRetries int
	// MaxResponseBytes bounds a response body (default 32 MiB).
	MaxResponseBytes int64
}

type osvBatchRequest struct {
	Queries []osvQuery `json:"queries"`
}

type osvQuery struct {
	Package   OSVPackage `json:"package"`
	Version   string     `json:"version,omitempty"`
	PageToken string     `json:"page_token,omitempty"`
}

type osvBatchResponse struct {
	Results []struct {
		Vulns []struct {
			ID       string `json:"id"`
			Modified string `json:"modified"`
		} `json:"vulns"`
		NextPageToken string `json:"next_page_token"`
	} `json:"results"`
}

func (c *OSVClient) base() string {
	if c.BaseURL == "" {
		return DefaultOSVBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *OSVClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// do performs an HTTP request with retries for transient failures.
func (c *OSVClient) do(ctx context.Context, method, u string, body []byte) ([]byte, error) {
	retries := c.MaxRetries
	if retries <= 0 {
		retries = 3
	}
	limit := c.MaxResponseBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	backoff := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		ua := c.UserAgent
		if ua == "" {
			ua = "gta-supplychain/1"
		}
		req.Header.Set("User-Agent", ua)
		resp, err := c.client().Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= retries {
				return nil, fmt.Errorf("osv: %s %s: %w", method, u, err)
			}
		} else {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
			resp.Body.Close()
			switch {
			case rerr != nil:
				if attempt >= retries {
					return nil, fmt.Errorf("osv: read %s: %w", u, rerr)
				}
			case int64(len(data)) > limit:
				return nil, fmt.Errorf("osv: response from %s exceeds %d bytes", u, limit)
			case resp.StatusCode == http.StatusOK:
				return data, nil
			case resp.StatusCode == http.StatusNotFound:
				return nil, fmt.Errorf("osv: %s: %w", u, fs.ErrNotExist)
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				if attempt >= retries {
					return nil, fmt.Errorf("osv: %s %s: status %d", method, u, resp.StatusCode)
				}
				if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 && ra <= 60 {
					backoff = time.Duration(ra) * time.Second
				}
			default:
				return nil, fmt.Errorf("osv: %s %s: status %d: %s", method, u, resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 512)])))
			}
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
	}
}

func queryCacheKey(q PackageQuery) string {
	return "osv-query\x00" + string(q.Ecosystem) + "\x00" + q.Name + "\x00" + q.Version
}

// QueryIDs returns the advisory IDs affecting each query via querybatch,
// following per-query pagination.
func (c *OSVClient) QueryIDs(ctx context.Context, qs []PackageQuery) ([][]string, error) {
	out := make([][]string, len(qs))
	type pending struct {
		idx   int
		token string
	}
	var work []pending
	for i, q := range qs {
		if raw, ok := c.Cache.Get(queryCacheKey(q)); ok {
			var ids []string
			if json.Unmarshal(raw, &ids) == nil {
				out[i] = ids
				continue
			}
		}
		work = append(work, pending{idx: i})
	}
	batch := c.BatchSize
	if batch <= 0 || batch > 1000 {
		batch = 1000
	}
	done := map[int]bool{}
	for len(work) > 0 {
		n := min(batch, len(work))
		chunk := work[:n]
		work = work[n:]
		req := osvBatchRequest{}
		for _, p := range chunk {
			q := qs[p.idx]
			req.Queries = append(req.Queries, osvQuery{
				Package:   OSVPackage{Ecosystem: string(q.Ecosystem), Name: q.Name},
				Version:   normalizeQueryVersion(q.Ecosystem, q.Version),
				PageToken: p.token,
			})
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		data, err := c.do(ctx, http.MethodPost, c.base()+"/v1/querybatch", body)
		if err != nil {
			return nil, err
		}
		var resp osvBatchResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("osv: decode querybatch: %w", err)
		}
		if len(resp.Results) != len(chunk) {
			return nil, fmt.Errorf("osv: querybatch returned %d results for %d queries", len(resp.Results), len(chunk))
		}
		for i, r := range resp.Results {
			p := chunk[i]
			for _, v := range r.Vulns {
				if v.ID != "" {
					out[p.idx] = append(out[p.idx], v.ID)
				}
			}
			if r.NextPageToken != "" {
				work = append(work, pending{idx: p.idx, token: r.NextPageToken})
			} else {
				done[p.idx] = true
			}
		}
	}
	for idx := range done {
		ids := dedupeSorted(out[idx])
		out[idx] = ids
		if raw, err := json.Marshal(ids); err == nil {
			_ = c.Cache.Put(queryCacheKey(qs[idx]), raw)
		}
	}
	return out, nil
}

func dedupeSorted(in []string) []string {
	if in == nil {
		return []string{}
	}
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// GetVuln fetches one advisory by ID (cached).
func (c *OSVClient) GetVuln(ctx context.Context, id string) (*OSVVulnerability, error) {
	key := "osv-vuln\x00" + id
	if raw, ok := c.Cache.Get(key); ok {
		var v OSVVulnerability
		if json.Unmarshal(raw, &v) == nil && v.ID == id {
			return &v, nil
		}
	}
	data, err := c.do(ctx, http.MethodGet, c.base()+"/v1/vulns/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var v OSVVulnerability
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("osv: decode %s: %w", id, err)
	}
	if v.ID != id {
		return nil, fmt.Errorf("osv: requested %s but received %q", id, v.ID)
	}
	_ = c.Cache.Put(key, data)
	return &v, nil
}

// Query implements VulnSource.
func (c *OSVClient) Query(ctx context.Context, qs []PackageQuery) ([][]*OSVVulnerability, error) {
	ids, err := c.QueryIDs(ctx, qs)
	if err != nil {
		return nil, err
	}
	unique := map[string]*OSVVulnerability{}
	for _, l := range ids {
		for _, id := range l {
			unique[id] = nil
		}
	}
	keys := make([]string, 0, len(unique))
	for k := range unique {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	conc := c.Concurrency
	if conc <= 0 {
		conc = 8
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, conc)
	)
	for _, id := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := c.GetVuln(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			unique[id] = v
		}(id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	out := make([][]*OSVVulnerability, len(qs))
	for i, l := range ids {
		for _, id := range l {
			if v := unique[id]; v != nil && v.Withdrawn == "" {
				out[i] = append(out[i], v)
			}
		}
	}
	return out, nil
}

// ---- Offline database ------------------------------------------------------

// OSVDatabase is an in-memory advisory index loaded from OSV JSON dumps
// (the per-ecosystem all.zip exports, extracted directories of *.json, or
// single files). It implements VulnSource without network access.
type OSVDatabase struct {
	byPkg map[string][]*OSVVulnerability
	ids   map[string]bool
}

// NewOSVDatabase returns an empty database.
func NewOSVDatabase() *OSVDatabase {
	return &OSVDatabase{byPkg: map[string][]*OSVVulnerability{}, ids: map[string]bool{}}
}

func osvIndexKey(eco, name string) string {
	base, _, _ := strings.Cut(eco, ":")
	if strings.EqualFold(base, string(EcosystemPyPI)) {
		name = NormalizePyPIName(name)
	}
	if strings.EqualFold(base, string(EcosystemPackagist)) {
		name = strings.ToLower(name)
	}
	return strings.ToLower(base) + "\x00" + name
}

// Add indexes an advisory. Duplicate IDs are ignored.
func (db *OSVDatabase) Add(v *OSVVulnerability) {
	if v == nil || v.ID == "" || db.ids[v.ID] {
		return
	}
	db.ids[v.ID] = true
	seen := map[string]bool{}
	for _, a := range v.Affected {
		k := osvIndexKey(a.Package.Ecosystem, a.Package.Name)
		if !seen[k] {
			seen[k] = true
			db.byPkg[k] = append(db.byPkg[k], v)
		}
	}
}

// Len returns the number of advisories.
func (db *OSVDatabase) Len() int { return len(db.ids) }

// LoadJSON adds one advisory (an OSV object) or a JSON array of them.
func (db *OSVDatabase) LoadJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var list []*OSVVulnerability
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		for _, v := range list {
			db.Add(v)
		}
		return nil
	}
	var v OSVVulnerability
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if v.ID == "" {
		return errors.New("osv: advisory without id")
	}
	db.Add(&v)
	return nil
}

// LoadPath loads a file (.json or .zip) or a directory tree of them.
func (db *OSVDatabase) LoadPath(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return db.loadFile(p)
	}
	return filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		return db.loadFile(fp)
	})
}

func (db *OSVDatabase) loadFile(p string) error {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".json":
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := db.LoadJSON(data); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	case ".zip":
		zr, err := zip.OpenReader(p)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if !strings.HasSuffix(strings.ToLower(f.Name), ".json") || f.FileInfo().IsDir() {
				continue
			}
			if f.UncompressedSize64 > 64<<20 {
				return fmt.Errorf("%s: %s: entry too large", p, f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(rc, 64<<20))
			rc.Close()
			if err != nil {
				return err
			}
			if err := db.LoadJSON(data); err != nil {
				return fmt.Errorf("%s: %s: %w", p, f.Name, err)
			}
		}
	}
	return nil
}

// Query implements VulnSource by evaluating ranges locally.
func (db *OSVDatabase) Query(ctx context.Context, qs []PackageQuery) ([][]*OSVVulnerability, error) {
	out := make([][]*OSVVulnerability, len(qs))
	for i, q := range qs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, v := range db.byPkg[osvIndexKey(string(q.Ecosystem), q.Name)] {
			a, _ := v.AffectedEntry(q.Ecosystem, q.Name, q.Version)
			if a != nil {
				out[i] = append(out[i], v)
			}
		}
	}
	return out, nil
}
