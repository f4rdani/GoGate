package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// LogEntry is a single structured log record held in the ring buffer.
type LogEntry struct {
	Time    string `json:"t"`
	Level   string `json:"l"`
	Message string `json:"m"`
	Attrs   string `json:"a,omitempty"`
}

// LogRing is a fixed-size circular buffer of recent log entries.
type LogRing struct {
	mu      sync.RWMutex
	entries []LogEntry
	cap     int
	head    int // index of next write slot
	size    int // how many entries are stored

	// SSE subscribers: map[id]chan LogEntry
	subs   map[uint64]chan LogEntry
	nextID uint64
	subsMu sync.Mutex
}

// GlobalLogRing is the singleton ring used by the slog handler.
var GlobalLogRing = &LogRing{
	cap:     500,
	entries: make([]LogEntry, 500),
	subs:    make(map[uint64]chan LogEntry),
}

// Push adds an entry to the ring and notifies all SSE subscribers.
func (r *LogRing) Push(e LogEntry) {
	r.mu.Lock()
	r.entries[r.head] = e
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
	r.mu.Unlock()

	// fan-out to SSE subscribers (non-blocking)
	r.subsMu.Lock()
	for _, ch := range r.subs {
		select {
		case ch <- e:
		default: // slow subscriber – drop
		}
	}
	r.subsMu.Unlock()
}

// Recent returns the last n entries in chronological order.
func (r *LogRing) Recent(n int) []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n > r.size {
		n = r.size
	}
	out := make([]LogEntry, n)
	start := (r.head - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		out[i] = r.entries[(start+i)%r.cap]
	}
	return out
}

// subscribe registers a new SSE subscriber and returns its ID and channel.
func (r *LogRing) subscribe() (uint64, chan LogEntry) {
	ch := make(chan LogEntry, 64)
	r.subsMu.Lock()
	id := r.nextID
	r.nextID++
	r.subs[id] = ch
	r.subsMu.Unlock()
	return id, ch
}

// unsubscribe removes a subscriber.
func (r *LogRing) unsubscribe(id uint64) {
	r.subsMu.Lock()
	delete(r.subs, id)
	r.subsMu.Unlock()
}

// ====== slog Handler ======

// RingHandler is a slog.Handler that writes to both a ring buffer and a delegate handler.
type RingHandler struct {
	delegate slog.Handler
	ring     *LogRing
	attrs    []slog.Attr
}

// NewRingHandler wraps an existing slog.Handler and additionally feeds entries to ring.
func NewRingHandler(delegate slog.Handler, ring *LogRing) *RingHandler {
	return &RingHandler{delegate: delegate, ring: ring}
}

func (h *RingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.delegate.Enabled(ctx, level)
}

func (h *RingHandler) Handle(ctx context.Context, r slog.Record) error {
	// Build attrs map
	attrsMap := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrsMap[a.Key] = fmt.Sprintf("%v", a.Value)
		return true
	})
	for _, a := range h.attrs {
		attrsMap[a.Key] = fmt.Sprintf("%v", a.Value)
	}

	// Suppress noisy health-poll 404s (e.g. Laragon pinging /json every second)
	if r.Message == "request" {
		path := attrsMap["path"]
		status := attrsMap["status"]
		if status == "404" && (path == "/json" || path == "/favicon.ico" || path == "/robots.txt") {
			return h.delegate.Handle(ctx, r)
		}
	}

	// Build attrs string for display
	var attrs string
	for k, v := range attrsMap {
		if attrs != "" {
			attrs += " "
		}
		attrs += k + "=" + v
	}

	entry := LogEntry{
		Time:    r.Time.Format("15:04:05.000"),
		Level:   r.Level.String(),
		Message: r.Message,
		Attrs:   attrs,
	}
	h.ring.Push(entry)
	return h.delegate.Handle(ctx, r)
}

func (h *RingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &RingHandler{delegate: h.delegate.WithAttrs(attrs), ring: h.ring, attrs: newAttrs}
}

func (h *RingHandler) WithGroup(name string) slog.Handler {
	return &RingHandler{delegate: h.delegate.WithGroup(name), ring: h.ring, attrs: h.attrs}
}

// ====== HTTP Handlers ======

// HandleGetLogs returns the last 200 log entries as JSON (GET /admin/logs).
func (a *AdminHandler) HandleGetLogs(w http.ResponseWriter, r *http.Request) {
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}
	entries := GlobalLogRing.Recent(200)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// HandleStreamLogs streams live log entries via Server-Sent Events (GET /admin/logs/stream).
// EventSource cannot set custom headers, so the secret may be passed via ?secret= query param.
func (a *AdminHandler) HandleStreamLogs(w http.ResponseWriter, r *http.Request) {
	// Allow secret via query string for EventSource (browsers can't set headers there)
	if qs := r.URL.Query().Get("secret"); qs != "" {
		r.Header.Set("X-Admin-Secret", qs)
	}
	if !a.checkAuth(r) {
		a.sendError(w, http.StatusUnauthorized, "Invalid admin secret")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send recent history first so the panel has context on connect
	for _, e := range GlobalLogRing.Recent(100) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	flusher.Flush()

	id, ch := GlobalLogRing.subscribe()
	defer GlobalLogRing.unsubscribe(id)

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case entry := <-ch:
			b, _ := json.Marshal(entry)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
