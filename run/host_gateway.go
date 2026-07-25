package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	pathpkg "path"
	"sort"
	"strings"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// ApplicationHTTPRuntime is the optional front-door contract implemented by
// an application runtime that owns an HTTP listener. The address is read only
// after Start succeeds; an empty value means the runtime is not ready.
//
// HTTPAddress may be either host:port or an absolute http(s) URL. A URL path is
// treated as the runtime's base path.
type ApplicationHTTPRuntime interface {
	ApplicationRuntime
	HTTPAddress() string
}

type hostGatewayRoute struct {
	prefix   string
	identity ApplicationIdentity
	target   ApplicationHTTPRuntime
}

// HostGateway is the single public HTTP front door for a pulp.host.toml
// composition. It maps canonical route prefixes to exact application instance
// identities and strips only the matched external prefix before proxying.
type HostGateway struct {
	mu     sync.Mutex
	addr   string
	logger *slog.Logger
	routes []hostGatewayRoute

	server   *http.Server
	listener net.Listener
	started  bool
	stopped  bool
}

// NewHostGateway resolves every manifest binding to one ready application
// runtime. It fails closed for missing, duplicate, unready, or non-HTTP
// runtimes, so a route can never silently fall through to a sibling app.
func NewHostGateway(addr string, bindings []*manifest.RouteBinding, runtimes []ApplicationRuntime, logger *slog.Logger) (*HostGateway, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("host gateway address is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	byIdentity := make(map[ApplicationIdentity]ApplicationRuntime, len(runtimes))
	for _, runtime := range runtimes {
		if runtime == nil {
			return nil, errors.New("host gateway runtime is nil")
		}
		identity := runtime.Identity()
		if err := identity.validate(); err != nil {
			return nil, fmt.Errorf("host gateway runtime identity: %w", err)
		}
		if _, exists := byIdentity[identity]; exists {
			return nil, fmt.Errorf("host gateway has duplicate runtime target %s", identity)
		}
		byIdentity[identity] = runtime
	}

	routes := make([]hostGatewayRoute, 0, len(bindings))
	seenPrefixes := make(map[string]ApplicationIdentity, len(bindings))
	for index, binding := range bindings {
		if binding == nil {
			return nil, fmt.Errorf("host gateway route %d is nil", index)
		}
		prefix, err := canonicalGatewayPrefix(binding.Path)
		if err != nil {
			return nil, fmt.Errorf("host gateway route %d: %w", index, err)
		}
		identity := ApplicationIdentity{ApplicationID: binding.Application, InstanceID: binding.Instance}
		if previous, exists := seenPrefixes[prefix]; exists {
			return nil, fmt.Errorf("host gateway route prefix %q is ambiguous between %s and %s", prefix, previous, identity)
		}
		seenPrefixes[prefix] = identity

		runtime, exists := byIdentity[identity]
		if !exists {
			return nil, fmt.Errorf("host gateway route %q target %s is unavailable", prefix, identity)
		}
		httpRuntime, ok := runtime.(ApplicationHTTPRuntime)
		if !ok {
			return nil, fmt.Errorf("host gateway route %q target %s does not expose HTTP", prefix, identity)
		}
		if _, err := parseGatewayUpstream(httpRuntime.HTTPAddress()); err != nil {
			return nil, fmt.Errorf("host gateway route %q target %s is unready: %w", prefix, identity, err)
		}

		route := hostGatewayRoute{prefix: prefix, identity: identity, target: httpRuntime}
		routes = append(routes, route)
	}
	// Longest prefix first makes nested bindings deterministic while exact
	// duplicate prefixes remain forbidden above.
	sort.Slice(routes, func(i, j int) bool {
		if len(routes[i].prefix) == len(routes[j].prefix) {
			return routes[i].prefix < routes[j].prefix
		}
		return len(routes[i].prefix) > len(routes[j].prefix)
	})

	gateway := &HostGateway{addr: addr, logger: logger, routes: routes}
	gateway.server = &http.Server{Handler: gateway}
	return gateway, nil
}

// NewSupervisorHostGateway binds a validated host manifest to the exact
// runtime instances currently owned by a running MultiHostSupervisor. The
// snapshot is taken under the supervisor lifecycle lock, preventing the
// gateway from observing a partially started or concurrently stopping host.
func NewSupervisorHostGateway(addr string, hostManifest *manifest.Host, supervisor *MultiHostSupervisor, logger *slog.Logger) (*HostGateway, error) {
	if hostManifest == nil {
		return nil, errors.New("host gateway manifest is required")
	}
	if supervisor == nil {
		return nil, errors.New("host gateway supervisor is required")
	}
	supervisor.mu.Lock()
	if supervisor.state != multiHostRunning {
		supervisor.mu.Unlock()
		return nil, errors.New("host gateway supervisor is not running")
	}
	runtimes := append([]ApplicationRuntime(nil), supervisor.runtimes...)
	supervisor.mu.Unlock()
	return NewHostGateway(addr, hostManifest.Routes, runtimes, logger)
}

func canonicalGatewayPrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#\\") {
		return "", fmt.Errorf("route prefix %q must be an absolute URL path", prefix)
	}
	if pathpkg.Clean(prefix) != prefix || (prefix != "/" && strings.HasSuffix(prefix, "/")) {
		return "", fmt.Errorf("route prefix %q must be canonical", prefix)
	}
	return prefix, nil
}

func parseGatewayUpstream(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("HTTP address is empty")
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	upstream, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse HTTP address: %w", err)
	}
	if (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, fmt.Errorf("HTTP address %q must be an http(s) origin with an optional base path", value)
	}
	return upstream, nil
}

func newHostGatewayProxy(route hostGatewayRoute, upstream *url.URL, logger *slog.Logger) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{}
	proxy.FlushInterval = -1
	proxy.Rewrite = func(request *httputil.ProxyRequest) {
		request.SetURL(upstream)
		remainder := stripGatewayPrefix(request.In.URL.Path, route.prefix)
		request.Out.URL.Path = joinGatewayPath(upstream.Path, remainder)
		escapedPrefix := (&url.URL{Path: route.prefix}).EscapedPath()
		escapedRemainder := stripGatewayPrefix(request.In.URL.EscapedPath(), escapedPrefix)
		request.Out.URL.RawPath = joinGatewayPath(upstream.EscapedPath(), escapedRemainder)
		if request.Out.URL.RawPath == request.Out.URL.Path {
			request.Out.URL.RawPath = ""
		}
		request.Out.Host = upstream.Host
		request.SetXForwarded()
		// These are host-owned routing assertions. A client cannot select or
		// impersonate a sibling application by injecting them.
		request.Out.Header.Set("X-Pulp-Application", route.identity.ApplicationID)
		request.Out.Header.Set("X-Pulp-Application-Instance", route.identity.InstanceID)
		request.Out.Header.Set("X-Forwarded-Prefix", route.prefix)
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		logger.Error("host gateway upstream failed", "path", request.URL.Path, "target", route.identity.String(), "err", err)
		http.Error(writer, "application upstream unavailable", http.StatusBadGateway)
	}
	return proxy
}

func stripGatewayPrefix(path, prefix string) string {
	if prefix == "/" {
		if path == "" {
			return "/"
		}
		return path
	}
	remainder := strings.TrimPrefix(path, prefix)
	if remainder == "" {
		return "/"
	}
	return remainder
}

func joinGatewayPath(base, path string) string {
	if base == "" || base == "/" {
		if path == "" {
			return "/"
		}
		return path
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

func (g *HostGateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	for index := range g.routes {
		route := &g.routes[index]
		if gatewayPrefixMatches(route.prefix, request.URL.Path) {
			upstream, err := parseGatewayUpstream(route.target.HTTPAddress())
			if err != nil {
				g.logger.Warn("host gateway target is unready", "target", route.identity.String(), "err", err)
				http.Error(writer, "application upstream unavailable", http.StatusServiceUnavailable)
				return
			}
			newHostGatewayProxy(*route, upstream, g.logger).ServeHTTP(writer, request)
			return
		}
	}
	http.NotFound(writer, request)
}

func gatewayPrefixMatches(prefix, path string) bool {
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// Start binds the gateway and begins serving. It returns after the listener is
// ready. Cancelling ctx triggers a graceful shutdown.
func (g *HostGateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return errors.New("host gateway is already started")
	}
	if g.stopped {
		g.mu.Unlock()
		return errors.New("host gateway is stopped")
	}
	listener, err := net.Listen("tcp", g.addr)
	if err != nil {
		g.mu.Unlock()
		return fmt.Errorf("host gateway listen %s: %w", g.addr, err)
	}
	g.listener = listener
	g.started = true
	g.mu.Unlock()

	go func() {
		if err := g.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			g.logger.Error("host gateway serve failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = g.Shutdown(context.Background())
	}()
	return nil
}

// Addr reports the bound listener address after Start. Before Start it returns
// the configured address.
func (g *HostGateway) Addr() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listener != nil {
		return g.listener.Addr().String()
	}
	return g.addr
}

// Shutdown gracefully stops the front door. It is idempotent.
func (g *HostGateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return nil
	}
	if !g.started {
		g.mu.Unlock()
		return nil
	}
	g.stopped = true
	server := g.server
	g.mu.Unlock()
	return server.Shutdown(ctx)
}
