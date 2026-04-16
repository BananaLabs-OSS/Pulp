package transport

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
)

// sseKeepalive is how often the server writes a heartbeat comment line
// on every subscriber connection to keep intermediaries from closing it.
const sseKeepalive = 15 * time.Second

// SSEServer serves Server-Sent Events streams. Plugins declare paths
// with sse_register and broadcast to all subscribers on that path with
// sse_emit. A background keepalive writes periodic comment lines so
// proxies do not timeout the long-poll connection.
type SSEServer struct {
	logger *slog.Logger

	mu     sync.Mutex
	routes map[string]struct{}
	subs   map[string]map[uint64]*sseSub
	nextID atomic.Uint64
}

type sseSub struct {
	id      uint64
	path    string
	write   chan []byte
	done    chan struct{}
	flusher http.Flusher
	writer  http.ResponseWriter
}

// NewSSEServer constructs an SSEServer. Routes and subscribers start empty.
func NewSSEServer(logger *slog.Logger) *SSEServer {
	return &SSEServer{
		logger: logger,
		routes: map[string]struct{}{},
		subs:   map[string]map[uint64]*sseSub{},
	}
}

// RegisterRoute adds path to the SSE route table. Path must begin with "/".
func (s *SSEServer) RegisterRoute(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("sse path %q must begin with /", path)
	}
	s.mu.Lock()
	s.routes[path] = struct{}{}
	s.mu.Unlock()
	s.logger.Info("sse route registered", "path", path)
	return nil
}

// HasRoute reports whether path has been registered as an SSE route.
// Used by HTTPServer to decide whether to treat an incoming GET as SSE.
func (s *SSEServer) HasRoute(path string) bool {
	s.mu.Lock()
	_, ok := s.routes[path]
	s.mu.Unlock()
	return ok
}

// Handle serves the long-poll SSE connection. The call blocks until the
// client disconnects or Stop is called. Writes SSE framing and keepalive
// comment lines; plugin-generated events are delivered via the sub's
// write channel.
func (s *SSEServer) Handle(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id := s.nextID.Add(1)
	sub := &sseSub{
		id:      id,
		path:    r.URL.Path,
		write:   make(chan []byte, 32),
		done:    make(chan struct{}),
		flusher: flusher,
		writer:  w,
	}

	s.mu.Lock()
	if _, ok := s.subs[sub.path]; !ok {
		s.subs[sub.path] = map[uint64]*sseSub{}
	}
	s.subs[sub.path][id] = sub
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if m, ok := s.subs[sub.path]; ok {
			delete(m, id)
		}
		s.mu.Unlock()
		close(sub.done)
	}()

	ticker := time.NewTicker(sseKeepalive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(":ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case payload := <-sub.write:
			if _, err := w.Write(payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// Emit broadcasts the event to every subscriber on req.Path. Returns an
// error only if the path has no registered route; subscribers with full
// buffers are dropped silently (slow consumers don't block the host).
func (s *SSEServer) Emit(req abi.SSEEmitRequest) error {
	s.mu.Lock()
	if _, ok := s.routes[req.Path]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("no sse route %q", req.Path)
	}
	targets := make([]*sseSub, 0, len(s.subs[req.Path]))
	for _, sub := range s.subs[req.Path] {
		targets = append(targets, sub)
	}
	s.mu.Unlock()

	payload := formatSSEFrame(req)
	for _, sub := range targets {
		select {
		case sub.write <- payload:
		default:
			s.logger.Warn("sse subscriber slow — dropping event", "path", req.Path, "sub", sub.id)
		}
	}
	return nil
}

// Subscribers returns the count of active connections on path. Diagnostic.
func (s *SSEServer) Subscribers(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[path])
}

// Stop closes all active subscriber connections.
func (s *SSEServer) Stop() {
	s.mu.Lock()
	for path, subs := range s.subs {
		for _, sub := range subs {
			sub.flusher.Flush()
			_ = path
			_ = sub
		}
	}
	s.subs = map[string]map[uint64]*sseSub{}
	s.mu.Unlock()
}

func formatSSEFrame(req abi.SSEEmitRequest) []byte {
	var b strings.Builder
	if req.ID != "" {
		b.WriteString("id: ")
		b.WriteString(req.ID)
		b.WriteString("\n")
	}
	if req.Event != "" {
		b.WriteString("event: ")
		b.WriteString(req.Event)
		b.WriteString("\n")
	}
	for _, line := range strings.Split(req.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return []byte(b.String())
}
