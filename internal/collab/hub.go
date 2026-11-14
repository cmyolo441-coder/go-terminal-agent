package collab

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Defaults for Options fields left zero.
const (
	DefaultMaxRooms          = 64
	DefaultMaxParticipants   = 64
	DefaultReplayBuffer      = 4096
	DefaultSubscriberBuffer  = 512
	DefaultKeepAlive         = 15 * time.Second
	DefaultWriteTimeout      = 10 * time.Second
	DefaultHeartbeatTimeout  = 30 * time.Second
	DefaultTypingTimeout     = 5 * time.Second
	DefaultTickInterval      = 250 * time.Millisecond
	DefaultRateLimit         = 30
	DefaultRateBurst         = 60
	DefaultStreamsPerUser    = 4
	DefaultChatHistory       = 200
	DefaultGCInterval        = 30 * time.Second
	DefaultReplicaEvictAfter = 10 * time.Minute
	DefaultRoomIdleTTL       = 30 * time.Minute
	DefaultInboxSize         = 256
	DefaultAuthFailRate      = 1
	DefaultAuthFailBurst     = 20
)

// ServerReplica is the replica id under which the room itself edits the
// shared documents.
const ServerReplica = "server"

// Errors returned by the Go API.
var (
	ErrRoomClosed   = errors.New("collab: room is closed")
	ErrRoomNotFound = errors.New("collab: room not found")
	ErrTooManyRooms = errors.New("collab: too many rooms")
	ErrHubClosed    = errors.New("collab: hub is closed")
)

// Options configure a Hub and the rooms it hosts.
type Options struct {
	// AdminToken authorises creating and listing rooms over HTTP. Empty
	// generates a random one (see Hub.AdminToken).
	AdminToken string
	// MaxRooms caps concurrently open rooms.
	MaxRooms int
	// MaxParticipants caps participants (invited, online or not) per room.
	MaxParticipants int
	// ReplayBuffer is how many sequenced events a room keeps for
	// Last-Event-ID resume.
	ReplayBuffer int
	// SubscriberBuffer is each stream's queue length. A stream whose
	// queue is full when a sequenced event arrives is disconnected with
	// reason slow_consumer (it may resume); ephemeral events are dropped.
	SubscriberBuffer int
	// KeepAlive is the interval of SSE comment pings.
	KeepAlive time.Duration
	// WriteTimeout bounds each write to a stream.
	WriteTimeout time.Duration
	// HeartbeatTimeout is how long a participant with no open stream
	// stays present without a message, and how long one with a stream
	// stays "active" before turning "idle".
	HeartbeatTimeout time.Duration
	// TypingTimeout clears a typing indicator that is not refreshed.
	TypingTimeout time.Duration
	// TickInterval paces the room's timers.
	TickInterval time.Duration
	// RateLimit (messages per second) and RateBurst bound each
	// participant's upstream messages. A negative RateLimit disables it.
	RateLimit float64
	RateBurst int
	// MaxStreamsPerParticipant caps concurrent event streams per
	// participant.
	MaxStreamsPerParticipant int
	// ChatHistory is how many chat messages a welcome carries.
	ChatHistory int
	// ViewersReadOnly forbids viewers from chatting.
	ViewersReadOnly bool
	// Quorum is the default approval policy for new rooms.
	Quorum QuorumPolicy
	// GCInterval paces tombstone collection and log compaction.
	GCInterval time.Duration
	// ReplicaEvictAfter drops an editor's replica from the stability
	// frontier after this long offline; the editor must resync on return.
	ReplicaEvictAfter time.Duration
	// RoomIdleTTL closes a room nobody has been present in for this long.
	// Negative keeps rooms until closed.
	RoomIdleTTL time.Duration
	// ClearOnSubmit empties the shared prompt after a submission.
	ClearOnSubmit bool
	// MaxDrift bounds how far ahead of server time a settings write may be
	// stamped.
	MaxDrift time.Duration
	// NewRecorder, when set, opens a recorder for each new room.
	NewRecorder func(roomID string) (Recorder, error)
	// OnSubmit receives prompt submissions; it runs on its own goroutine.
	OnSubmit func(room *Room, by ParticipantInfo, text string)
	// Logger receives lifecycle logs; nil discards them.
	Logger *slog.Logger
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

func (o *Options) withDefaults() error {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.MaxRooms, DefaultMaxRooms)
	def(&o.MaxParticipants, DefaultMaxParticipants)
	def(&o.ReplayBuffer, DefaultReplayBuffer)
	def(&o.SubscriberBuffer, DefaultSubscriberBuffer)
	def(&o.MaxStreamsPerParticipant, DefaultStreamsPerUser)
	def(&o.ChatHistory, DefaultChatHistory)
	def(&o.RateBurst, DefaultRateBurst)
	if o.RateLimit == 0 {
		o.RateLimit = DefaultRateLimit
	}
	defDur(&o.KeepAlive, DefaultKeepAlive)
	defDur(&o.WriteTimeout, DefaultWriteTimeout)
	defDur(&o.HeartbeatTimeout, DefaultHeartbeatTimeout)
	defDur(&o.TypingTimeout, DefaultTypingTimeout)
	defDur(&o.TickInterval, DefaultTickInterval)
	defDur(&o.GCInterval, DefaultGCInterval)
	defDur(&o.ReplicaEvictAfter, DefaultReplicaEvictAfter)
	defDur(&o.MaxDrift, DefaultMaxDrift)
	if o.RoomIdleTTL == 0 {
		o.RoomIdleTTL = DefaultRoomIdleTTL
	}
	if o.Quorum.Approve == nil {
		o.Quorum = DefaultQuorumPolicy()
	}
	if err := o.Quorum.Validate(); err != nil {
		return err
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AdminToken == "" {
		o.AdminToken = newToken()
	}
	return nil
}

// Hub hosts rooms and serves them over HTTP (see Handler).
type Hub struct {
	opts      Options
	adminHash [32]byte
	log       *slog.Logger
	authFail  *failLimiter

	mu     sync.Mutex
	rooms  map[string]*Room
	closed bool
	wg     sync.WaitGroup
}

// NewHub validates options and returns a hub.
func NewHub(o Options) (*Hub, error) {
	if err := o.withDefaults(); err != nil {
		return nil, err
	}
	return &Hub{
		opts:      o,
		adminHash: sha256.Sum256([]byte(o.AdminToken)),
		log:       o.Logger,
		authFail:  newFailLimiter(DefaultAuthFailRate, DefaultAuthFailBurst),
		rooms:     map[string]*Room{},
	}, nil
}

// AdminToken returns the token that authorises room creation.
func (h *Hub) AdminToken() string { return h.opts.AdminToken }

// Credentials let one participant into one room.
type Credentials struct {
	RoomID        string `json:"room_id"`
	ParticipantID string `json:"participant_id"`
	Token         string `json:"token"`
	Role          Role   `json:"role"`
}

// RoomConfig describes a new room.
type RoomConfig struct {
	Title     string        `json:"title,omitempty"`
	OwnerName string        `json:"owner_name,omitempty"`
	Policy    *QuorumPolicy `json:"policy,omitempty"`
}

// CreateRoom opens a room and returns its owner's credentials.
func (h *Hub) CreateRoom(cfg RoomConfig) (*Room, Credentials, error) {
	policy := h.opts.Quorum
	if cfg.Policy != nil {
		if err := cfg.Policy.Validate(); err != nil {
			return nil, Credentials{}, err
		}
		policy = *cfg.Policy
	}
	title, err := cleanName(cfg.Title, "untitled", 200)
	if err != nil {
		return nil, Credentials{}, err
	}
	owner, err := cleanName(cfg.OwnerName, "owner", MaxNameRunes)
	if err != nil {
		return nil, Credentials{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, Credentials{}, ErrHubClosed
	}
	if len(h.rooms) >= h.opts.MaxRooms {
		return nil, Credentials{}, ErrTooManyRooms
	}
	id := "r_" + randomHex(9)
	var rec Recorder
	if h.opts.NewRecorder != nil {
		if rec, err = h.opts.NewRecorder(id); err != nil {
			return nil, Credentials{}, fmt.Errorf("collab: open recorder: %w", err)
		}
	}
	r := newRoom(h, id, title, policy, rec)
	creds, err := r.addParticipant(owner, RoleOwner)
	if err != nil {
		return nil, Credentials{}, err
	}
	h.rooms[id] = r
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		r.run()
		h.mu.Lock()
		if h.rooms[id] == r {
			delete(h.rooms, id)
		}
		h.mu.Unlock()
	}()
	h.log.Info("collab room created", "room", id, "title", title)
	return r, creds, nil
}

// Room returns an open room.
func (h *Hub) Room(id string) (*Room, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[id]
	return r, ok
}

// Rooms describes every open room.
func (h *Hub) Rooms(ctx context.Context) []RoomInfo {
	h.mu.Lock()
	rooms := slices.Collect(maps.Values(h.rooms))
	h.mu.Unlock()
	out := make([]RoomInfo, 0, len(rooms))
	for _, r := range rooms {
		if info, err := r.Info(ctx); err == nil {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(a, b RoomInfo) int { return a.Created.Compare(b.Created) })
	return out
}

// Close closes every room and waits for them to stop.
func (h *Hub) Close() error {
	h.mu.Lock()
	h.closed = true
	rooms := slices.Collect(maps.Values(h.rooms))
	h.mu.Unlock()
	for _, r := range rooms {
		r.Close("server shutting down")
	}
	h.wg.Wait()
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("collab: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("collab: crypto/rand failed: " + err.Error())
	}
	return "gtc_" + base64.RawURLEncoding.EncodeToString(b)
}

// cleanName trims and validates a display name.
func cleanName(s, fallback string, maxRunes int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback, nil
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxRunes {
		return "", fmt.Errorf("collab: name must be at most %d runes of UTF-8", maxRunes)
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return "", errors.New("collab: name must not contain control characters")
		}
	}
	return s, nil
}

// failLimiter throttles repeated authentication failures per client
// address.
type failLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   int
	buckets map[string]*tokenBucket
}

func newFailLimiter(rate float64, burst int) *failLimiter {
	return &failLimiter{rate: rate, burst: burst, buckets: map[string]*tokenBucket{}}
}

// blocked reports whether key has exhausted its failure budget.
func (f *failLimiter) blocked(key string, now time.Time) (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[key]
	if !ok {
		return false, 0
	}
	probe := *b
	if allowed, wait := probe.allow(now); !allowed {
		return true, wait
	}
	return false, 0
}

// fail charges one failure to key.
func (f *failLimiter) fail(key string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[key]
	if !ok {
		if len(f.buckets) >= 10000 {
			f.prune(now)
		}
		nb := newTokenBucket(f.rate, f.burst, now)
		b = &nb
		f.buckets[key] = b
	}
	b.allow(now)
}

func (f *failLimiter) prune(now time.Time) {
	for k, b := range f.buckets {
		if now.Sub(b.last).Seconds()*b.rate+b.tokens >= b.burst {
			delete(f.buckets, k)
		}
	}
}
