package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Parity upstream-body capture.
//
// The gateway defaults unset sampler fields from the runtime profile before
// forwarding to the engine. A dual-arm parity run that does not observe the
// merc arm's on-the-wire body can only assert request identity, not prove it.
// When MERC_PARITY_CAPTURE_UPSTREAM=1 the gateway retains recent upstream
// bodies keyed by request id and exposes a localhost-only read path so the
// harness can assert byte equality against the direct arm.
//
// This path MUST NOT exist in production. The handler is registered only when
// the env flag is set, and even then refuses non-loopback peers. Never enable
// the flag on a shared or internet-facing control plane.

const parityUpstreamCaptureEnv = "MERC_PARITY_CAPTURE_UPSTREAM"

type parityUpstreamEntry struct {
	RequestID string `json:"request_id"`
	SHA256    string `json:"sha256"`
	Body      []byte `json:"body"`
}

type parityUpstreamCapture struct {
	mu      sync.RWMutex
	byID    map[string]parityUpstreamEntry
	order   []string
	maxKeep int
}

func newParityUpstreamCapture(maxKeep int) *parityUpstreamCapture {
	if maxKeep < 1 {
		maxKeep = 64
	}
	return &parityUpstreamCapture{
		byID:    make(map[string]parityUpstreamEntry, maxKeep),
		maxKeep: maxKeep,
	}
}

func (c *parityUpstreamCapture) put(requestID string, body []byte) {
	if c == nil || requestID == "" || len(body) == 0 {
		return
	}
	sum := sha256.Sum256(body)
	entry := parityUpstreamEntry{
		RequestID: requestID,
		SHA256:    hex.EncodeToString(sum[:]),
		Body:      append([]byte(nil), body...),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.byID[requestID]; !exists {
		c.order = append(c.order, requestID)
	}
	c.byID[requestID] = entry
	for len(c.order) > c.maxKeep {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.byID, old)
	}
}

func (c *parityUpstreamCapture) get(requestID string) (parityUpstreamEntry, bool) {
	if c == nil {
		return parityUpstreamEntry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.byID[requestID]
	return e, ok
}

func (c *parityUpstreamCapture) latest() (parityUpstreamEntry, bool) {
	if c == nil {
		return parityUpstreamEntry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.order) == 0 {
		return parityUpstreamEntry{}, false
	}
	id := c.order[len(c.order)-1]
	e, ok := c.byID[id]
	return e, ok
}

var (
	parityCaptureOnce sync.Once
	parityCapture     *parityUpstreamCapture
)

// parityCaptureEnabled is true only when the env flag is exactly "1".
func parityCaptureEnabled() bool {
	return os.Getenv(parityUpstreamCaptureEnv) == "1"
}

func parityCaptureStore() *parityUpstreamCapture {
	if !parityCaptureEnabled() {
		return nil
	}
	parityCaptureOnce.Do(func() {
		parityCapture = newParityUpstreamCapture(128)
	})
	return parityCapture
}

// captureParityUpstreamBody records the body the gateway is about to put on
// the wire to the engine. No-op unless capture is enabled.
func captureParityUpstreamBody(requestID string, body []byte) {
	if store := parityCaptureStore(); store != nil {
		store.put(requestID, body)
	}
}

// parityUpstreamBodySHA returns the sha256 of a previously captured upstream
// body, or "" when capture is off / unknown.
func parityUpstreamBodySHA(requestID string) string {
	store := parityCaptureStore()
	if store == nil {
		return ""
	}
	e, ok := store.get(requestID)
	if !ok {
		return ""
	}
	return e.SHA256
}

// registerParityCaptureRoutes mounts the localhost-only debug reader. Call
// only when building Routes(); the handler itself re-checks the env flag and
// peer address so a mis-registered route still refuses.
func (s *Server) registerParityCaptureRoutes(mux *http.ServeMux) {
	if !parityCaptureEnabled() {
		return
	}
	mux.HandleFunc("GET /v1/internal/parity/upstream-body", s.handleParityUpstreamBody)
}

func (s *Server) handleParityUpstreamBody(w http.ResponseWriter, r *http.Request) {
	if !parityCaptureEnabled() {
		http.NotFound(w, r)
		return
	}
	if !isLoopbackRequest(r) {
		writeOpenAIError(w, http.StatusForbidden,
			"parity upstream capture is loopback-only",
			"invalid_request_error", "parity_capture_remote_forbidden")
		return
	}
	store := parityCaptureStore()
	if store == nil {
		http.NotFound(w, r)
		return
	}
	requestID := strings.TrimSpace(r.URL.Query().Get("request_id"))
	var (
		entry parityUpstreamEntry
		ok    bool
	)
	if requestID == "" {
		entry, ok = store.latest()
	} else {
		entry, ok = store.get(requestID)
	}
	if !ok {
		writeOpenAIError(w, http.StatusNotFound,
			"no captured upstream body for the requested id",
			"invalid_request_error", "parity_capture_miss")
		return
	}
	// Return the raw body as application/json (it is the upstream JSON) with
	// identity headers the harness asserts against.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Merc-Upstream-Body-SHA256", entry.SHA256)
	w.Header().Set("X-Merc-Request-ID", entry.RequestID)
	w.Header().Set("X-Merc-Parity-Capture", "1")
	// Also emit a small envelope when ?envelope=1 so a skeptic can see metadata
	// without re-hashing.
	if r.URL.Query().Get("envelope") == "1" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": entry.RequestID,
			"sha256":     entry.SHA256,
			"body_utf8":  string(entry.Body),
			"bytes":      len(entry.Body),
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.Body)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
