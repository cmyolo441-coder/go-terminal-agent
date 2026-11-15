// Package session persists conversations as JSONL transcripts so they can be resumed.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
)

type record struct {
	Type    string          `json:"type"` // meta | message | reset | title
	ID      string          `json:"id,omitempty"`
	Cwd     string          `json:"cwd,omitempty"`
	Time    time.Time       `json:"time,omitempty"`
	Title   string          `json:"title,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
}

// Session is an append-only transcript file.
type Session struct {
	ID    string
	Cwd   string
	Path  string
	title bool
}

var reSlug = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// Dir returns the transcript directory for a project.
func Dir(cwd string) string {
	return filepath.Join(config.Home(), "projects", strings.Trim(reSlug.ReplaceAllString(cwd, "-"), "-"))
}

// New creates a session; the file is created on the first write.
func New(cwd string) *Session {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:4])
	return &Session{ID: id, Cwd: cwd, Path: filepath.Join(Dir(cwd), id+".jsonl")}
}

func (s *Session) write(recs ...record) error {
	if s == nil {
		return nil
	}
	_, statErr := os.Stat(s.Path)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if statErr != nil {
		recs = append([]record{{Type: "meta", ID: s.ID, Cwd: s.Cwd, Time: time.Now()}}, recs...)
	}
	w := bufio.NewWriter(f)
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	return w.Flush()
}

// Append records one conversation message.
func (s *Session) Append(msg json.RawMessage) error {
	return s.write(record{Type: "message", Message: msg})
}

// SetTitle records the first prompt as the session title, once.
func (s *Session) SetTitle(title string) {
	if s == nil || s.title || strings.TrimSpace(title) == "" {
		return
	}
	s.title = true
	title = strings.Join(strings.Fields(title), " ")
	if r := []rune(title); len(r) > 100 {
		title = string(r[:100])
	}
	_ = s.write(record{Type: "title", Title: title})
}

// Reset records that the history was replaced (after compaction or /clear).
func (s *Session) Reset(msgs []json.RawMessage) error {
	recs := []record{{Type: "reset", Time: time.Now()}}
	for _, m := range msgs {
		recs = append(recs, record{Type: "message", Message: m})
	}
	return s.write(recs...)
}

// Load reads a transcript and returns a session that appends to it, plus its messages.
func Load(path string) (*Session, []json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	s := &Session{Path: path, ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	var msgs []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue // tolerate a torn final line
		}
		switch r.Type {
		case "meta":
			s.Cwd = r.Cwd
		case "title":
			s.title = true
		case "reset":
			msgs = nil
		case "message":
			msgs = append(msgs, r.Message)
		}
	}
	return s, msgs, sc.Err()
}

// Info summarizes a stored session.
type Info struct {
	ID       string
	Path     string
	Title    string
	Modified time.Time
	Messages int
}

// List returns the project's sessions, newest first.
func List(cwd string) []Info {
	entries, _ := os.ReadDir(Dir(cwd))
	var out []Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(Dir(cwd), e.Name())
		info := Info{ID: strings.TrimSuffix(e.Name(), ".jsonl"), Path: p}
		if st, err := e.Info(); err == nil {
			info.Modified = st.ModTime()
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 256<<20)
		for sc.Scan() {
			var r struct {
				Type  string `json:"type"`
				Title string `json:"title"`
			}
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			switch r.Type {
			case "title":
				info.Title = r.Title
			case "message":
				info.Messages++
			case "reset":
				info.Messages = 0
			}
		}
		f.Close()
		if info.Messages > 0 {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// Find resolves a session id (or unique prefix) for the project.
func Find(cwd, id string) (Info, error) {
	var hits []Info
	for _, s := range List(cwd) {
		if s.ID == id {
			return s, nil
		}
		if strings.HasPrefix(s.ID, id) || strings.HasSuffix(s.ID, id) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 0:
		return Info{}, fmt.Errorf("no session matching %q in this project", id)
	case 1:
		return hits[0], nil
	}
	return Info{}, fmt.Errorf("%q matches %d sessions; give more of the id", id, len(hits))
}
