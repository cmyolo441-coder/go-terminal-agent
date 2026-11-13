package codeintel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The cache file is a fixed header followed by a gzip-compressed gob
// payload:
//
//	magic[8] | version uint32 | payload length uint64 | sha256(payload)[32] | payload
//
// The checksum guards against truncated or corrupted files; any mismatch
// discards the cache and the index is rebuilt from source.
const (
	cacheMagic   = "GTACIDX\x00"
	cacheVersion = 3
	// analysisVersion changes whenever extraction or chunking changes, so
	// old entries are never reused with new semantics.
	analysisVersion = "2026.11.3"
	maxCacheSize    = 1 << 30
	cacheHeaderSize = 8 + 4 + 8 + sha256.Size
)

type cachePayload struct {
	Root     string
	Key      string
	SavedAt  int64
	Files    []*fileEntry
	Skipped  map[string]int
	Truncate bool
}

// cacheKey identifies the settings that shape index content.
func (ix *Index) cacheKey() string {
	return fmt.Sprintf("%s|max=%d|gen=%t|extra=%s", analysisVersion, ix.opts.MaxFileSize, ix.opts.IncludeGenerated,
		strings.Join(ix.opts.ExtraIgnore, "\x1f"))
}

// CachePath is the cache file for this index, or "" without a cache dir.
func (ix *Index) CachePath() string {
	if ix.opts.CacheDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ix.root))
	return filepath.Join(ix.opts.CacheDir, "codeintel-"+hex.EncodeToString(sum[:8])+".idx")
}

// saveCache writes the snapshot atomically: a temp file in the same
// directory is synced and renamed over the old cache.
func (ix *Index) saveCache(s *snapshot) error {
	p := ix.CachePath()
	if p == "" {
		return nil
	}
	files := make([]*fileEntry, 0, len(s.all))
	for _, e := range s.all {
		files = append(files, e)
	}
	var payload bytes.Buffer
	zw, err := gzip.NewWriterLevel(&payload, gzip.BestSpeed)
	if err != nil {
		return err
	}
	err = gob.NewEncoder(zw).Encode(cachePayload{
		Root: ix.root, Key: ix.cacheKey(), SavedAt: time.Now().UnixNano(),
		Files: files, Skipped: s.walkSkipped, Truncate: s.stats.Truncated,
	})
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if payload.Len() > maxCacheSize {
		return fmt.Errorf("index too large to cache (%d bytes)", payload.Len())
	}
	header := make([]byte, 0, cacheHeaderSize)
	header = append(header, cacheMagic...)
	header = binary.BigEndian.AppendUint32(header, cacheVersion)
	header = binary.BigEndian.AppendUint64(header, uint64(payload.Len()))
	sum := sha256.Sum256(payload.Bytes())
	header = append(header, sum[:]...)

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".codeintel-*.tmp")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(header); err != nil {
		return err
	}
	if _, err := tmp.Write(payload.Bytes()); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	ok = true
	return nil
}

// errCacheMiss marks a cache that is absent or belongs to other settings,
// which is not worth logging.
var errCacheMiss = errors.New("cache miss")

// readCache loads and verifies the cache file.
func (ix *Index) readCache() (*cachePayload, error) {
	p := ix.CachePath()
	if p == "" {
		return nil, errCacheMiss
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errCacheMiss
		}
		return nil, err
	}
	defer f.Close()
	header := make([]byte, cacheHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("short header: %w", err)
	}
	if string(header[:8]) != cacheMagic {
		return nil, fmt.Errorf("not a codeintel cache")
	}
	if v := binary.BigEndian.Uint32(header[8:12]); v != cacheVersion {
		return nil, errCacheMiss
	}
	n := binary.BigEndian.Uint64(header[12:20])
	if n == 0 || n > maxCacheSize {
		return nil, fmt.Errorf("bad payload length %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(f, payload); err != nil {
		return nil, fmt.Errorf("truncated payload: %w", err)
	}
	if extra, _ := f.Read(make([]byte, 1)); extra != 0 {
		return nil, fmt.Errorf("trailing data")
	}
	sum := sha256.Sum256(payload)
	if !bytes.Equal(sum[:], header[20:]) {
		return nil, fmt.Errorf("checksum mismatch")
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var cp cachePayload
	if err := gob.NewDecoder(io.LimitReader(zr, 8*maxCacheSize)).Decode(&cp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if cp.Root != ix.root || cp.Key != ix.cacheKey() {
		return nil, errCacheMiss
	}
	seen := make(map[string]bool, len(cp.Files))
	for _, e := range cp.Files {
		if err := e.validate(); err != nil {
			return nil, err
		}
		if seen[e.Path] {
			return nil, fmt.Errorf("duplicate entry %s", e.Path)
		}
		seen[e.Path] = true
	}
	return &cp, nil
}

// loadCache seeds the index from disk. The loaded snapshot is served
// immediately and validated by the next refresh, which re-reads only files
// whose size or mtime differ.
func (ix *Index) loadCache() {
	cp, err := ix.readCache()
	if err != nil {
		if !errors.Is(err, errCacheMiss) {
			ix.logf("codeintel: ignoring cache %s: %v", ix.CachePath(), err)
		}
		return
	}
	all := make(map[string]*fileEntry, len(cp.Files))
	for _, e := range cp.Files {
		// The cache may predate Exclude (a policy installed since).
		if ix.excluded(e.Path, false) {
			continue
		}
		ix.intern.internEntry(e)
		all[e.Path] = e
	}
	snap := buildSnapshot(ix.root, all, cp.Skipped, cp.Truncate)
	snap.stats.FromCache = true
	snap.stats.Reused = len(all)
	snap.stats.LastRefresh = time.Unix(0, cp.SavedAt)
	ix.snap.Store(snap)
}
