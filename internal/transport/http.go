// Package transport implements Pulp's transport primitives.
//
// v0.3 begins with HTTP inbound: plugins declare transport.http.inbound,
// register routes during pulp_init, and receive requests via the step
// envelope payload. A request enqueued by the HTTP goroutine is handed
// to the plugin on the next step call; the plugin must call http_respond
// within that same step — if it does not, the host answers the client
// with 500 and the drop is logged.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
)

const defaultRequestTimeout = 30 * time.Second

// HTTPServer is Pulp's inbound HTTP dispatcher. One per host.
//
// Lifecycle:
//  1. NewHTTPServer — constructed before the plugin loads.
//  2. Plugin calls http_register during pulp_init — routes accumulate.
//  3. Start — binds the listener, begins accepting requests.
//  4. Step loop drains PopRequest and hands the request to the plugin.
//  5. Plugin calls http_respond; Finalize backstops missing responses.
//  6. Stop — graceful shutdown.
type HTTPServer struct {
	addr   string
	logger *slog.Logger

	mu      sync.Mutex
	routes  []route
	pending map[uint64]*inflightRequest
	nextID  atomic.Uint64

	queue  chan *inflightRequest
	server *http.Server

	certPath string
	keyPath  string
}

type route struct {
	method string
	parts  []pathPart
}

type pathPart struct {
	literal string
	param   string
}

type inflightRequest struct {
	req    abi.HTTPRequest
	respCh chan abi.HTTPResponse
}

// NewHTTPServer constructs an HTTPServer bound to addr (e.g. ":8080").
// The server is not listening until Start is called.
func NewHTTPServer(addr string, logger *slog.Logger) *HTTPServer {
	return &HTTPServer{
		addr:    addr,
		logger:  logger,
		pending: map[uint64]*inflightRequest{},
		queue:   make(chan *inflightRequest, 64),
	}
}

// EnableTLS configures HTTPS. certPath and keyPath are PEM files; the
// pair is validated immediately via tls.LoadX509KeyPair. Call before
// Start. Subsequent requests to the server will require TLS.
func (s *HTTPServer) EnableTLS(certPath, keyPath string) error {
	if strings.TrimSpace(certPath) == "" || strings.TrimSpace(keyPath) == "" {
		return errors.New("both certPath and keyPath are required")
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("load tls cert/key: %w", err)
	}
	s.certPath = certPath
	s.keyPath = keyPath
	s.logger.Info("http tls enabled", "cert", certPath)
	return nil
}

// RegisterRoute adds method+pattern to the route table. :param segments
// capture path parameters. Returns an error if the pattern is malformed.
func (s *HTTPServer) RegisterRoute(method, pattern string) error {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return errors.New("method is required")
	}
	if !strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("pattern %q must begin with /", pattern)
	}
	parts := parsePattern(pattern)

	s.mu.Lock()
	s.routes = append(s.routes, route{method: method, parts: parts})
	s.mu.Unlock()
	s.logger.Info("http route registered", "method", method, "pattern", pattern)
	return nil
}

// Start binds the HTTP listener and begins accepting requests. Returns
// once the server goroutine has started.
func (s *HTTPServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.dispatch)

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}
	useTLS := s.certPath != "" && s.keyPath != ""
	go func() {
		var err error
		if useTLS {
			err = s.server.ListenAndServeTLS(s.certPath, s.keyPath)
		} else {
			err = s.server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http listen failed", "err", err)
		}
	}()
	s.logger.Info("http server started", "addr", s.addr, "tls", useTLS)
	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *HTTPServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// PopRequest returns the next pending HTTP request without blocking.
// Returns (zero, false) if the queue is empty.
func (s *HTTPServer) PopRequest() (abi.HTTPRequest, bool) {
	select {
	case ir := <-s.queue:
		return ir.req, true
	default:
		return abi.HTTPRequest{}, false
	}
}

// Respond delivers a plugin-produced HTTPResponse to the waiting HTTP
// handler. Called from the http_respond host import on the step-loop
// goroutine.
func (s *HTTPServer) Respond(resp abi.HTTPResponse) error {
	s.mu.Lock()
	ir, ok := s.pending[resp.ID]
	if ok {
		delete(s.pending, resp.ID)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending request id %d", resp.ID)
	}
	ir.respCh <- resp
	return nil
}

// Finalize backstops requests the plugin did not answer during its step.
// The step loop must call Finalize after every step whose envelope carried
// an HTTP request, passing the request ID. If the plugin already called
// http_respond for that ID this is a no-op.
func (s *HTTPServer) Finalize(id uint64) {
	s.mu.Lock()
	ir, still := s.pending[id]
	if still {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if still {
		s.logger.Warn("plugin did not respond", "id", id)
		ir.respCh <- abi.HTTPResponse{
			ID:     id,
			Status: 500,
			Body:   []byte("plugin did not respond"),
		}
	}
}

// Queued reports the number of requests waiting to be handed to the plugin.
func (s *HTTPServer) Queued() int { return len(s.queue) }

// dispatch matches an incoming request to a registered route, enqueues it
// for the step loop, and blocks on the plugin's response (or a timeout).
func (s *HTTPServer) dispatch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	routes := s.routes
	s.mu.Unlock()

	var match *route
	var params map[string]string
	for i := range routes {
		if routes[i].method != r.Method {
			continue
		}
		p, ok := matchPattern(routes[i].parts, r.URL.Path)
		if !ok {
			continue
		}
		match = &routes[i]
		params = p
		break
	}
	if match == nil {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	id := s.nextID.Add(1)
	headers := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	query := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			query[k] = vs[0]
		}
	}

	ir := &inflightRequest{
		req: abi.HTTPRequest{
			ID:      id,
			Method:  r.Method,
			Path:    r.URL.Path,
			Params:  params,
			Query:   query,
			Headers: headers,
			Body:    body,
		},
		respCh: make(chan abi.HTTPResponse, 1),
	}

	s.mu.Lock()
	s.pending[id] = ir
	s.mu.Unlock()

	select {
	case s.queue <- ir:
	default:
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		http.Error(w, "queue full", http.StatusServiceUnavailable)
		return
	}

	select {
	case resp := <-ir.respCh:
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		status := int(resp.Status)
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(resp.Body)
	case <-time.After(defaultRequestTimeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		http.Error(w, "plugin timeout", http.StatusGatewayTimeout)
	}
}

func parsePattern(pattern string) []pathPart {
	segments := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	parts := make([]pathPart, len(segments))
	for i, seg := range segments {
		if strings.HasPrefix(seg, ":") {
			parts[i] = pathPart{param: strings.TrimPrefix(seg, ":")}
		} else {
			parts[i] = pathPart{literal: seg}
		}
	}
	return parts
}

func matchPattern(parts []pathPart, path string) (map[string]string, bool) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) != len(parts) {
		return nil, false
	}
	params := map[string]string{}
	for i, p := range parts {
		if p.param != "" {
			params[p.param] = segments[i]
			continue
		}
		if p.literal != segments[i] {
			return nil, false
		}
	}
	return params, true
}
