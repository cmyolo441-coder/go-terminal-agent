package collab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Handler serves the collaboration API:
//
//	POST   /v1/collab/rooms                          create a room (admin token) {"title","owner_name","policy"}
//	GET    /v1/collab/rooms                          list rooms (admin token)
//	GET    /v1/collab/rooms/{room}                   room, caller and participants (participant token)
//	DELETE /v1/collab/rooms/{room}                   close the room (owner or admin token)
//	POST   /v1/collab/rooms/{room}/participants      invite {"name","role"} (owner) -> credentials
//	GET    /v1/collab/rooms/{room}/events            SSE stream; resume with Last-Event-ID
//	POST   /v1/collab/rooms/{room}/messages          upstream ClientMessage -> Reply
//
// Every request but room creation and listing authenticates with a
// participant token as "Authorization: Bearer <token>"; the events stream
// also accepts ?access_token= because browsers' EventSource cannot set
// headers. Tokens are compared in constant time, and repeated failures
// from one address are throttled.
//
// Downstream traffic is Server-Sent Events, which pass through ordinary
// HTTP proxies; upstream traffic is plain POSTs. Each sequenced event's
// SSE id is its room sequence number, so a reconnecting client (or the
// browser, automatically) sends Last-Event-ID and receives exactly the
// events it missed, as long as they are still in the replay buffer.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/collab/rooms", h.handleCreate)
	mux.HandleFunc("GET /v1/collab/rooms", h.handleList)
	mux.HandleFunc("GET /v1/collab/rooms/{room}", h.handleRoom)
	mux.HandleFunc("DELETE /v1/collab/rooms/{room}", h.handleClose)
	mux.HandleFunc("POST /v1/collab/rooms/{room}/participants", h.handleInvite)
	mux.HandleFunc("GET /v1/collab/rooms/{room}/events", h.handleEvents)
	mux.HandleFunc("POST /v1/collab/rooms/{room}/messages", h.handleMessage)
	return h.recoverer(mux)
}

func (h *Hub) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				h.log.Error("collab handler panic", "panic", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, &Error{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, e *Error) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gta-collab"`)
	}
	var body apiErrorBody
	body.Error.Code, body.Error.Message = e.Code, e.Message
	writeJSON(w, e.Status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func bearer(r *http.Request, allowQuery bool) string {
	const prefix = "bearer "
	if h := r.Header.Get("Authorization"); len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	if allowQuery {
		return r.URL.Query().Get("access_token")
	}
	return ""
}

// throttled rejects clients that failed authentication too often.
func (h *Hub) throttled(w http.ResponseWriter, r *http.Request) bool {
	if blocked, wait := h.authFail.blocked(clientAddr(r), h.opts.Now()); blocked {
		writeError(w, &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many failed authentication attempts", RetryAfter: wait})
		return true
	}
	return false
}

func (h *Hub) authFailed(w http.ResponseWriter, r *http.Request, e *Error) {
	if e.Status == http.StatusUnauthorized {
		h.authFail.fail(clientAddr(r), h.opts.Now())
	}
	writeError(w, e)
}

func (h *Hub) isAdmin(r *http.Request) bool {
	tok := bearer(r, false)
	if tok == "" {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], h.adminHash[:]) == 1
}

func (h *Hub) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.throttled(w, r) {
		return false
	}
	if !h.isAdmin(r) {
		h.authFailed(w, r, &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "invalid admin token"})
		return false
	}
	return true
}

func (h *Hub) room(w http.ResponseWriter, r *http.Request) (*Room, bool) {
	if h.throttled(w, r) {
		return nil, false
	}
	room, ok := h.Room(r.PathValue("room"))
	if !ok {
		writeError(w, &Error{Status: http.StatusNotFound, Code: "not_found", Message: "no such room"})
		return nil, false
	}
	return room, true
}

// readBody reads a bounded JSON body.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *Error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return nil, &Error{Status: http.StatusUnsupportedMediaType, Code: "bad_content_type", Message: "send the body as application/json"}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &Error{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Message: fmt.Sprintf("request body is larger than %d bytes", limit)}
		}
		return nil, &Error{Status: http.StatusBadRequest, Code: "bad_request", Message: "could not read the request body"}
	}
	return body, nil
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	body, e := readBody(w, r, 64<<10)
	if e != nil {
		writeError(w, e)
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if err := strictUnmarshal(body, v); err != nil {
		writeError(w, &Error{Status: http.StatusBadRequest, Code: "bad_request", Message: "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

// CreateRoomResponse is the reply to room creation.
type CreateRoomResponse struct {
	Room  RoomInfo    `json:"room"`
	Owner Credentials `json:"owner"`
}

func (h *Hub) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var cfg RoomConfig
	if !decodeStrict(w, r, &cfg) {
		return
	}
	room, creds, err := h.CreateRoom(cfg)
	switch {
	case errors.Is(err, ErrTooManyRooms):
		writeError(w, &Error{Status: http.StatusServiceUnavailable, Code: "too_many_rooms", Message: err.Error()})
		return
	case errors.Is(err, ErrHubClosed):
		writeError(w, &Error{Status: http.StatusServiceUnavailable, Code: "shutting_down", Message: err.Error()})
		return
	case err != nil:
		writeError(w, &Error{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()})
		return
	}
	info, err := room.Info(r.Context())
	if err != nil {
		writeError(w, errClosed)
		return
	}
	writeJSON(w, http.StatusCreated, CreateRoomResponse{Room: info, Owner: creds})
}

func (h *Hub) handleList(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": h.Rooms(r.Context())})
}

// RoomView is the reply to GET /v1/collab/rooms/{room}.
type RoomView struct {
	Room         RoomInfo          `json:"room"`
	You          ParticipantInfo   `json:"you"`
	Participants []ParticipantInfo `json:"participants"`
}

func (h *Hub) handleRoom(w http.ResponseWriter, r *http.Request) {
	room, ok := h.room(w, r)
	if !ok {
		return
	}
	me, info, list, e := room.whoami(r.Context(), bearer(r, false))
	if e != nil {
		h.authFailed(w, r, e)
		return
	}
	writeJSON(w, http.StatusOK, RoomView{Room: info, You: me, Participants: list})
}

func (h *Hub) handleClose(w http.ResponseWriter, r *http.Request) {
	room, ok := h.room(w, r)
	if !ok {
		return
	}
	reason := "closed by an administrator"
	if !h.isAdmin(r) {
		me, _, _, e := room.whoami(r.Context(), bearer(r, false))
		if e != nil {
			h.authFailed(w, r, e)
			return
		}
		if me.Role != RoleOwner {
			writeError(w, &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "only an owner may close the room"})
			return
		}
		reason = "closed by " + me.Name
	}
	room.Close(reason)
	<-room.Done()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// InviteRequest is the body of an invitation.
type InviteRequest struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
}

func (h *Hub) handleInvite(w http.ResponseWriter, r *http.Request) {
	room, ok := h.room(w, r)
	if !ok {
		return
	}
	var req InviteRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	creds, e := room.inviteAs(r.Context(), bearer(r, false), req.Name, req.Role)
	if e != nil {
		h.authFailed(w, r, e)
		return
	}
	writeJSON(w, http.StatusCreated, creds)
}

func (h *Hub) handleMessage(w http.ResponseWriter, r *http.Request) {
	room, ok := h.room(w, r)
	if !ok {
		return
	}
	token := bearer(r, false)
	if token == "" {
		h.authFailed(w, r, errUnauthorized)
		return
	}
	body, e := readBody(w, r, MaxMessageBytes)
	if e != nil {
		writeError(w, e)
		return
	}
	reply, err := room.Handle(r.Context(), token, body)
	if err != nil {
		var ae *Error
		if !errors.As(err, &ae) {
			ae = &Error{Status: http.StatusInternalServerError, Code: "internal", Message: err.Error()}
		}
		h.authFailed(w, r, ae)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func parseLastEventID(r *http.Request) (uint64, bool) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("last_event_id")
	}
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	return n, err == nil
}

func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	room, ok := h.room(w, r)
	if !ok {
		return
	}
	lastID, resume := parseLastEventID(r)
	sub, backlog, e := room.subscribe(r.Context(), bearer(r, true), lastID, resume)
	if e != nil {
		h.authFailed(w, r, e)
		return
	}
	defer room.unsubscribe(sub)

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache, no-store")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	write := func(frames ...[]byte) error {
		_ = rc.SetWriteDeadline(time.Now().Add(h.opts.WriteTimeout))
		for _, f := range frames {
			if _, err := w.Write(f); err != nil {
				return err
			}
		}
		return rc.Flush()
	}
	if err := write(append([][]byte{[]byte("retry: 2000\n\n")}, backlog...)...); err != nil {
		return
	}
	keepAlive := time.NewTicker(h.opts.KeepAlive)
	defer keepAlive.Stop()
	batch := make([][]byte, 0, 64)
	for {
		select {
		case f := <-sub.ch:
			// Coalesce whatever else is queued into one flush.
			batch = append(batch[:0], f)
		more:
			for len(batch) < cap(batch) {
				select {
				case f := <-sub.ch:
					batch = append(batch, f)
				default:
					break more
				}
			}
			if err := write(batch...); err != nil {
				return
			}
		case <-sub.done:
			var frames [][]byte
			if sub.reason.Reason != ReasonSlowConsumer {
			drain:
				for {
					select {
					case f := <-sub.ch:
						frames = append(frames, f)
					default:
						break drain
					}
				}
			}
			frames = append(frames, encodeFrame(Event{Type: EvDisconnect, Time: time.Now().UTC(), Data: mustJSON(sub.reason)}))
			_ = write(frames...)
			return
		case <-keepAlive.C:
			if err := write([]byte(": ping\n\n")); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// ServeContext is a convenience for embedding: it serves Handler on ln
// until ctx ends, then shuts the server and the hub down.
func (h *Hub) ServeContext(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		_ = h.Close()
		return err
	case <-ctx.Done():
	}
	_ = h.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
