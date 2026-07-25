package run

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

type hostGatewayTestRuntime struct {
	identity ApplicationIdentity
	mu       sync.RWMutex
	address  string
}

func (r *hostGatewayTestRuntime) Identity() ApplicationIdentity  { return r.identity }
func (r *hostGatewayTestRuntime) Start(context.Context) error    { return nil }
func (r *hostGatewayTestRuntime) Shutdown(context.Context) error { return nil }
func (r *hostGatewayTestRuntime) HTTPAddress() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.address
}

func (r *hostGatewayTestRuntime) setHTTPAddress(address string) {
	r.mu.Lock()
	r.address = address
	r.mu.Unlock()
}

func TestHostGatewayRoutesExactApplicationInstancesAndStripsPrefix(t *testing.T) {
	evolutionUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.EscapedPath(), "/internal/player%2Fone"; got != want {
			t.Errorf("Evolution upstream escaped path = %q, want %q", got, want)
		}
		if request.URL.RawQuery != "include=state" {
			t.Errorf("Evolution upstream query = %q", request.URL.RawQuery)
		}
		if got := request.Header.Get("X-Pulp-Application"); got != "evolution" {
			t.Errorf("Evolution application header = %q", got)
		}
		if got := request.Header.Get("X-Pulp-Application-Instance"); got != "primary" {
			t.Errorf("Evolution instance header = %q", got)
		}
		if got := request.Header.Get("X-Forwarded-Prefix"); got != "/evolution" {
			t.Errorf("Evolution forwarded prefix = %q", got)
		}
		_, _ = writer.Write([]byte("evolution"))
	}))
	defer evolutionUpstream.Close()
	sessionsUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal" {
			t.Errorf("Sessions upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("X-Pulp-Application"); got != "sessions" {
			t.Errorf("Sessions application header = %q", got)
		}
		_, _ = writer.Write([]byte("sessions"))
	}))
	defer sessionsUpstream.Close()

	runtimes := []ApplicationRuntime{
		&hostGatewayTestRuntime{identity: ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}, address: evolutionUpstream.URL},
		&hostGatewayTestRuntime{identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}, address: sessionsUpstream.URL},
	}
	bindings := []*manifest.RouteBinding{
		{Path: "/sessions", Application: "sessions", Instance: "blue"},
		{Path: "/evolution", Application: "evolution", Instance: "primary"},
	}
	gateway := hostGatewayTestStart(t, bindings, runtimes)

	request, err := http.NewRequest(http.MethodGet, "http://"+gateway.Addr()+"/evolution/internal/player%2Fone?include=state", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Host routing assertions are not client-controlled.
	request.Header.Set("X-Pulp-Application", "sessions")
	request.Header.Set("X-Pulp-Application-Instance", "blue")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Evolution gateway request: %v", err)
	}
	if body := hostGatewayTestBody(t, response); body != "evolution" {
		t.Fatalf("Evolution gateway body = %q", body)
	}

	response, err = http.Get("http://" + gateway.Addr() + "/sessions/internal")
	if err != nil {
		t.Fatalf("Sessions gateway request: %v", err)
	}
	if body := hostGatewayTestBody(t, response); body != "sessions" {
		t.Fatalf("Sessions gateway body = %q", body)
	}

	response, err = http.Get("http://" + gateway.Addr() + "/session/internal")
	if err != nil {
		t.Fatalf("unbound gateway request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unbound route status = %d, want 404", response.StatusCode)
	}
}

func TestHostGatewayPreservesStreamingHeadersAndBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Add("Set-Cookie", "one=1; Path=/")
		writer.Header().Add("Set-Cookie", "two=2; Path=/")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: ready\ndata: one\n\n"))
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = writer.Write([]byte("event: done\ndata: two\n\n"))
	}))
	defer upstream.Close()

	gateway := hostGatewayTestStart(t,
		[]*manifest.RouteBinding{{Path: "/events", Application: "sessions", Instance: "blue"}},
		[]ApplicationRuntime{&hostGatewayTestRuntime{
			identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"},
			address:  upstream.URL,
		}},
	)
	response, err := http.Get("http://" + gateway.Addr() + "/events/stream")
	if err != nil {
		t.Fatalf("streaming gateway request: %v", err)
	}
	body := hostGatewayTestBody(t, response)
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("stream content type = %q", got)
	}
	if got := response.Header.Values("Set-Cookie"); len(got) != 2 {
		t.Fatalf("stream Set-Cookie headers = %#v", got)
	}
	if !strings.Contains(body, "event: ready") || !strings.Contains(body, "event: done") {
		t.Fatalf("stream body = %q", body)
	}
}

func TestHostGatewayRejectsAmbiguousUnreadyAndCrossApplicationTargets(t *testing.T) {
	ready := &hostGatewayTestRuntime{
		identity: ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"},
		address:  "127.0.0.1:12345",
	}
	duplicate := []*manifest.RouteBinding{
		{Path: "/app", Application: "evolution", Instance: "primary"},
		{Path: "/app", Application: "sessions", Instance: "blue"},
	}
	if _, err := NewHostGateway("127.0.0.1:0", duplicate, []ApplicationRuntime{ready}, nil); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate route error = %v, want ambiguous", err)
	}

	missing := []*manifest.RouteBinding{{Path: "/sessions", Application: "sessions", Instance: "blue"}}
	if _, err := NewHostGateway("127.0.0.1:0", missing, []ApplicationRuntime{ready}, nil); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("cross-application target error = %v, want unavailable", err)
	}

	unready := &hostGatewayTestRuntime{
		identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"},
	}
	if _, err := NewHostGateway("127.0.0.1:0", missing, []ApplicationRuntime{unready}, nil); err == nil || !strings.Contains(err.Error(), "unready") {
		t.Fatalf("unready target error = %v, want unready", err)
	}
}

func TestNewSupervisorHostGatewayRequiresRunningExactRuntimeSnapshot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()
	runtime := &hostGatewayTestRuntime{
		identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"},
		address:  upstream.URL,
	}
	hostManifest := &manifest.Host{Routes: []*manifest.RouteBinding{
		{Path: "/sessions", Application: "sessions", Instance: "blue"},
	}}
	supervisor := &MultiHostSupervisor{state: multiHostRunning, runtimes: []ApplicationRuntime{runtime}}
	gateway, err := NewSupervisorHostGateway("127.0.0.1:0", hostManifest, supervisor, nil)
	if err != nil {
		t.Fatalf("NewSupervisorHostGateway: %v", err)
	}
	if len(gateway.routes) != 1 || gateway.routes[0].identity != runtime.identity {
		t.Fatalf("gateway routes = %#v", gateway.routes)
	}

	supervisor.state = multiHostStopped
	if _, err := NewSupervisorHostGateway("127.0.0.1:0", hostManifest, supervisor, nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped supervisor error = %v, want not running", err)
	}
}

func TestHostGatewayGracefulLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	runtime := &hostGatewayTestRuntime{
		identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"},
		address:  upstream.URL,
	}
	gateway, err := NewHostGateway("127.0.0.1:0", []*manifest.RouteBinding{
		{Path: "/sessions", Application: "sessions", Instance: "blue"},
	}, []ApplicationRuntime{runtime}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown before start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := gateway.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start after pre-start shutdown: %v", err)
	}
	if err := gateway.Start(ctx); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("duplicate start error = %v", err)
	}
	if err := gateway.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := gateway.Shutdown(context.Background()); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
	cancel()
}

func TestHostGatewayTracksEndpointLossAndRecovery(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("first"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("second"))
	}))
	defer second.Close()
	runtime := &hostGatewayTestRuntime{
		identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"},
		address:  first.URL,
	}
	gateway := hostGatewayTestStart(t,
		[]*manifest.RouteBinding{{Path: "/sessions", Application: "sessions", Instance: "blue"}},
		[]ApplicationRuntime{runtime},
	)

	response, err := http.Get("http://" + gateway.Addr() + "/sessions/internal")
	if err != nil {
		t.Fatal(err)
	}
	if body := hostGatewayTestBody(t, response); body != "first" {
		t.Fatalf("initial endpoint body = %q", body)
	}

	runtime.setHTTPAddress("")
	response, err = http.Get("http://" + gateway.Addr() + "/sessions/internal")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("lost endpoint status = %d, want 503", response.StatusCode)
	}

	runtime.setHTTPAddress(second.URL)
	response, err = http.Get("http://" + gateway.Addr() + "/sessions/internal")
	if err != nil {
		t.Fatal(err)
	}
	if body := hostGatewayTestBody(t, response); body != "second" {
		t.Fatalf("recovered endpoint body = %q", body)
	}
}

func hostGatewayTestStart(t *testing.T, bindings []*manifest.RouteBinding, runtimes []ApplicationRuntime) *HostGateway {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateway, err := NewHostGateway("127.0.0.1:0", bindings, runtimes, logger)
	if err != nil {
		t.Fatalf("NewHostGateway: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := gateway.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start gateway: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = gateway.Shutdown(context.Background())
	})
	return gateway
}

func hostGatewayTestBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, body = %s", response.StatusCode, body)
	}
	return string(body)
}
