package audit

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// HMACKey verifies hmac-sha256 records. Without it such records fail.
	HMACKey []byte
	// RequireHMAC rejects plain sha256 records, so an attacker cannot
	// rewrite a keyed log as an unkeyed one.
	RequireHMAC bool
	// Anchor is the expected PrevHash of the first record (default
	// Genesis). Set it to a previously published head to verify a log
	// whose older segments were archived.
	Anchor string
	// AnchorSeq is the sequence number of the anchored record (0 for
	// Genesis).
	AnchorSeq uint64
	// AllowPrunedHead accepts whatever the first record links to, for logs
	// whose oldest segments were removed and no anchor is known.
	AllowPrunedHead bool
}

// Break describes the first broken link in a chain.
type Break struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Seq    uint64 `json:"seq,omitempty"`
	Reason string `json:"reason"`
}

func (b Break) String() string {
	return fmt.Sprintf("%s:%d (seq %d): %s", b.File, b.Line, b.Seq, b.Reason)
}

// Report is the result of a verification.
type Report struct {
	Files    int    `json:"files"`
	Records  int    `json:"records"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	// LastHash is the head of the verified chain.
	LastHash string `json:"last_hash"`
	// Broken is the first problem found; nil means the chain is intact.
	Broken *Break `json:"broken,omitempty"`
}

// OK reports whether the chain verified.
func (r Report) OK() bool { return r.Broken == nil }

// maxLine bounds one record when reading, so a corrupt file cannot make the
// verifier allocate without limit.
const maxLine = 16 << 20

// Verify checks every segment in dir, in order, as one chain. The error is
// for I/O problems; a tampered log yields a Report with Broken set.
func Verify(dir string, opts VerifyOptions) (Report, error) {
	segs, err := listSegments(dir)
	if err != nil {
		return Report{}, err
	}
	v := &verifier{opts: opts, prev: opts.Anchor, seq: opts.AnchorSeq}
	if v.prev == "" {
		v.prev = Genesis
	}
	for _, s := range segs {
		f, err := os.Open(filepath.Join(dir, s.name))
		if err != nil {
			return v.rep, fmt.Errorf("audit: %w", err)
		}
		v.rep.Files++
		err = v.file(s.name, f)
		f.Close()
		if err != nil || v.rep.Broken != nil {
			return v.rep, err
		}
	}
	return v.rep, nil
}

// VerifyReader checks one stream of records as a chain starting at
// opts.Anchor.
func VerifyReader(name string, r io.Reader, opts VerifyOptions) (Report, error) {
	v := &verifier{opts: opts, prev: opts.Anchor, seq: opts.AnchorSeq}
	if v.prev == "" {
		v.prev = Genesis
	}
	v.rep.Files = 1
	err := v.file(name, r)
	return v.rep, err
}

type verifier struct {
	opts    VerifyOptions
	prev    string
	seq     uint64
	started bool
	rep     Report
}

var hashSuffixLen = len(`,"hash":"`) + 64 + len(`"}`)

func (v *verifier) file(name string, r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for lineNo := 1; ; lineNo++ {
		line, err := readLine(br)
		if err == io.EOF && len(line) == 0 {
			return nil
		}
		fail := func(seq uint64, reason string) error {
			v.rep.Broken = &Break{File: name, Line: lineNo, Seq: seq, Reason: reason}
			return nil
		}
		if err == errLineTooLong {
			return fail(0, "record longer than 16 MiB")
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("audit: %s: %w", name, err)
		}
		if err == io.EOF {
			return fail(0, "incomplete final record (no newline)")
		}
		if len(line) < hashSuffixLen+2 || !bytes.HasPrefix(line[len(line)-hashSuffixLen:], []byte(`,"hash":"`)) || !bytes.HasSuffix(line, []byte(`"}`)) {
			return fail(0, "record has no trailing hash field")
		}
		var rec struct {
			Seq      uint64 `json:"seq"`
			Alg      string `json:"alg"`
			PrevHash string `json:"prev_hash"`
			Hash     string `json:"hash"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return fail(0, "malformed record: "+err.Error())
		}
		stated := line[len(line)-hashSuffixLen+len(`,"hash":"`) : len(line)-2]
		body := append(append([]byte{}, line[:len(line)-hashSuffixLen]...), '}')
		var key []byte
		switch rec.Alg {
		case AlgSHA256:
			if v.opts.RequireHMAC {
				return fail(rec.Seq, "record is not HMAC-signed")
			}
		case AlgHMACSHA256:
			if len(v.opts.HMACKey) == 0 {
				return fail(rec.Seq, "record is HMAC-signed; verification needs the key")
			}
			key = v.opts.HMACKey
		default:
			return fail(rec.Seq, fmt.Sprintf("unknown hash algorithm %q", rec.Alg))
		}
		want := digest(rec.Alg, key, body)
		if subtle.ConstantTimeCompare([]byte(want), stated) != 1 || rec.Hash != string(stated) {
			return fail(rec.Seq, "hash mismatch: the record was modified")
		}
		if !v.started && v.opts.AllowPrunedHead {
			v.prev, v.seq = rec.PrevHash, rec.Seq-1
		}
		if rec.PrevHash != v.prev {
			return fail(rec.Seq, "prev_hash does not match the previous record: a record was removed, inserted or reordered")
		}
		if rec.Seq != v.seq+1 {
			return fail(rec.Seq, fmt.Sprintf("sequence jumps from %d to %d", v.seq, rec.Seq))
		}
		if !v.started {
			v.rep.FirstSeq = rec.Seq
			v.started = true
		}
		v.prev, v.seq = want, rec.Seq
		v.rep.Records++
		v.rep.LastSeq, v.rep.LastHash = rec.Seq, want
	}
}

var errLineTooLong = errors.New("line too long")

// readLine returns one line without its newline; io.EOF with data means
// the last line had no newline.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		part, err := br.ReadSlice('\n')
		out = append(out, part...)
		if len(out) > maxLine {
			return nil, errLineTooLong
		}
		switch err {
		case nil:
			return out[:len(out)-1], nil
		case bufio.ErrBufferFull:
			continue
		default:
			return out, err
		}
	}
}

// Read calls fn for every record in dir, oldest first, without verifying
// the chain. It stops at the first malformed line or error from fn.
func Read(dir string, fn func(Record) error) error {
	segs, err := listSegments(dir)
	if err != nil {
		return err
	}
	for _, s := range segs {
		if err := readFile(filepath.Join(dir, s.name), fn); err != nil {
			return err
		}
	}
	return nil
}

func readFile(path string, fn func(Record) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	for lineNo := 1; ; lineNo++ {
		line, err := readLine(br)
		if err == io.EOF && len(line) == 0 {
			return nil
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("audit: %s:%d: %w", filepath.Base(path), lineNo, err)
		}
		var rec Record
		if jerr := json.Unmarshal(line, &rec); jerr != nil {
			return fmt.Errorf("audit: %s:%d: %w", filepath.Base(path), lineNo, jerr)
		}
		if ferr := fn(rec); ferr != nil {
			return ferr
		}
		if err == io.EOF {
			return nil
		}
	}
}
