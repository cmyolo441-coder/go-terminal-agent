// Package session persists conversations so they can be resumed.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// Session is one saved conversation.
type Session struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Root     string        `json:"root"`
	Model    string        `json:"model"`
	Created  time.Time     `json:"created"`
	Updated  time.Time     `json:"updated"`
	Usage    llm.Usage     `json:"usage"`
	CostUSD  float64       `json:"cost_usd"`
	Messages []llm.Message `json:"messages"`

	// enc remembers how each message was last encoded, so saving a long
	// conversation after a new turn only encodes the new messages.
	enc *encodeCache
}

// Store reads and writes sessions in a directory.
type Store struct{ Dir string }

// DefaultStore uses <user config dir>/sessions.
func DefaultStore() *Store { return &Store{Dir: filepath.Join(config.UserDir(), "sessions")} }

// New creates an unsaved session.
func New(root, model string) *Session {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	now := time.Now().UTC()
	return &Session{
		ID:      now.Format("20060102-150405") + "-" + hex.EncodeToString(b)[:6],
		Root:    root,
		Model:   model,
		Created: now,
		Updated: now,
	}
}

// Save writes a session atomically with owner-only permissions.
func (s *Store) Save(sess *Session) error {
	if len(sess.Messages) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	sess.Updated = time.Now().UTC()
	if sess.Title == "" {
		sess.Title = titleFrom(sess.Messages)
	}
	if sess.enc == nil {
		sess.enc = &encodeCache{}
	}
	data, err := sess.enc.marshal(sess)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir, sess.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a session by ID or unique ID prefix.
func (s *Store) Load(id string) (*Session, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no saved sessions")
		}
		return nil, err
	}
	var match []string
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if name == id {
			match = []string{name}
			break
		}
		if strings.HasPrefix(name, id) {
			match = append(match, name)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no session matches %q", id)
	case 1:
	default:
		return nil, fmt.Errorf("%q matches %d sessions; use more characters", id, len(match))
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, match[0]+".json"))
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("session %s is corrupt: %w", match[0], err)
	}
	return &sess, nil
}

// Summary is a listing row.
type Summary struct {
	ID       string
	Title    string
	Root     string
	Updated  time.Time
	Messages int
}

// List returns sessions newest first. When root is set, only sessions for
// that workspace are returned.
func (s *Store) List(root string) ([]Summary, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		var sess Session
		if json.Unmarshal(data, &sess) != nil {
			continue
		}
		if root != "" && sess.Root != root {
			continue
		}
		out = append(out, Summary{ID: sess.ID, Title: sess.Title, Root: sess.Root, Updated: sess.Updated, Messages: len(sess.Messages)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Latest returns the most recent session for a workspace.
func (s *Store) Latest(root string) (*Session, error) {
	list, err := s.List(root)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no saved sessions for %s", root)
	}
	return s.Load(list[0].ID)
}

func titleFrom(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			if t := strings.Join(strings.Fields(m.Text()), " "); t != "" {
				if len([]rune(t)) > 60 {
					t = string([]rune(t)[:59]) + "…"
				}
				return t
			}
		}
	}
	return "untitled"
}
