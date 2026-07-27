package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	pulpThreeAppResolverProvider20260725 = "minecraft-resolver.route.v1"
	pulpThreeAppInternalSecret20260725   = "${INTERNAL_SECRET}"
)

type pulpThreeAppResolverCall20260725 struct {
	Cell     string
	Provider string
	Method   string
	Path     string
}

type pulpThreeAppRouteRequest20260725 struct {
	Method  string            `msgpack:"method"`
	Path    string            `msgpack:"path"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

type pulpThreeAppRouteResponse20260725 struct {
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

// TestPulpThreeApplicationResolverAppCall20260725 boots the exact production
// Resolver -> Sessions -> Evolution host graph from source-built WASM cells.
// Only Evolution receives a public HTTP pump. Its protected internal health
// route must reach Resolver through the scoped pulp_app_call_v1 registry; no
// standalone Resolver host route, endpoint pump, or Resolver URL is available.
func TestPulpThreeApplicationResolverAppCall20260725(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real three-application Resolver AppCall E2E in short mode")
	}

	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	hostPath := filepath.Join(workspace, "Evolution", "pulp.host.toml")
	hostManifest, err := manifest.LoadHost(hostPath)
	if err != nil {
		t.Fatalf("load production three-application host: %v", err)
	}
	assertPulpThreeAppResolverHostShape20260725(t, hostManifest)

	applications, err := (ManifestHostLoader{}).LoadHostApplications(context.Background(), hostPath)
	if err != nil {
		t.Fatalf("load hosted applications: %v", err)
	}
	if len(applications) != 3 ||
		applications[0].Identity != (ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}) ||
		applications[1].Identity != (ApplicationIdentity{ApplicationID: "minecraft-resolver", InstanceID: "primary"}) ||
		applications[2].Identity != (ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}) ||
		len(applications[2].DependsOn) != 2 ||
		applications[2].DependsOn[0] != "minecraft-resolver" ||
		applications[2].DependsOn[1] != "sessions" {
		t.Fatalf("production hosted application order = %#v", applications)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	baseCapabilities := evolutionAppCapabilities()
	cache, storageRoot := t.TempDir(), t.TempDir()
	endpoints := NewEndpointRegistry()
	crossApplications := newCrossApplicationRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)

	started := make([]*evolutionHostedApplication, 0, len(applications))
	t.Cleanup(func() {
		for index := len(started) - 1; index >= 0; index-- {
			started[index].close(context.Background())
		}
	})
	for _, application := range applications {
		started = append(started, startEvolutionHostedApplication(
			t, ctx, workspace, cache, storageRoot, endpoints, crossApplications,
			baseCapabilities, logger, application,
		))
	}

	if _, ok := endpoints.ApplicationAddress(
		"minecraft-resolver", "primary", "transport.http.inbound", "public",
	); ok {
		t.Fatal("Resolver unexpectedly started a standalone public HTTP endpoint")
	}
	if _, ok := endpoints.ApplicationAddress(
		"sessions", "primary", "transport.http.inbound", "public",
	); ok {
		t.Fatal("Sessions unexpectedly started a standalone public HTTP endpoint")
	}

	var callsMu sync.Mutex
	var calls []pulpThreeAppResolverCall20260725
	resolverIdentity := ApplicationIdentity{ApplicationID: "minecraft-resolver", InstanceID: "primary"}
	crossApplications.mu.RLock()
	resolverEntry := crossApplications.entries[resolverIdentity]
	crossApplications.mu.RUnlock()
	if resolverEntry == nil || resolverEntry.invoke == nil {
		t.Fatal("Resolver application was not registered for scoped AppCall")
	}
	originalInvoke := resolverEntry.invoke
	resolverEntry.invoke = func(ctx context.Context, cell, provider string, request []byte) ([]byte, error) {
		var route struct {
			Method string `msgpack:"method"`
			Path   string `msgpack:"path"`
		}
		if err := msgpack.Unmarshal(request, &route); err != nil {
			return nil, fmt.Errorf("decode recorded Resolver AppCall: %w", err)
		}
		callsMu.Lock()
		calls = append(calls, pulpThreeAppResolverCall20260725{
			Cell: cell, Provider: provider, Method: route.Method, Path: route.Path,
		})
		callsMu.Unlock()
		return originalInvoke(ctx, cell, provider, request)
	}

	evolution := started[2]
	baseAddress, ok := endpoints.ApplicationAddress(
		"evolution", "primary", "transport.http.inbound", "public",
	)
	if !ok {
		t.Fatal("Evolution did not publish its sole public host endpoint")
	}
	startEvolutionAppHTTPPump(t, ctx, evolution.cells["evolution"].cell, baseCapabilities["transport.http.inbound"])

	request, err := http.NewRequest(http.MethodGet, "http://"+baseAddress+"/internal/resolver/health", nil)
	if err != nil {
		t.Fatalf("build Evolution Resolver health request: %v", err)
	}
	request.Header.Set("X-Internal-Secret", pulpThreeAppInternalSecret20260725)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET Evolution -> Resolver health: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Evolution -> Resolver health: %v", err)
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode Evolution -> Resolver health status %d body %s: %v", response.StatusCode, body, err)
	}
	if response.StatusCode != http.StatusOK || fmt.Sprint(health["status"]) != "ok" {
		t.Fatalf("Evolution -> Resolver health = %d %s", response.StatusCode, body)
	}

	preflightRequest, err := msgpack.Marshal(pulpThreeAppRouteRequest20260725{
		Method: http.MethodPost,
		Path:   "/preflight/jre",
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		// An unknown engine intentionally resolves no external JAR URLs. The
		// request still must cross Resolver -> jvm-jre-detect /scan and return
		// that sibling provider's compatible empty-scan projection.
		Body: []byte(`{"engine":"runtime-proof"}`),
	})
	if err != nil {
		t.Fatalf("encode JRE preflight AppCall: %v", err)
	}
	preflightWire, err := resolverEntry.invoke(
		ctx, "minecraft-resolver", pulpThreeAppResolverProvider20260725, preflightRequest,
	)
	if err != nil {
		t.Fatalf("Resolver -> JRE sibling-provider preflight: %v", err)
	}
	var preflightResponse pulpThreeAppRouteResponse20260725
	if err := msgpack.Unmarshal(preflightWire, &preflightResponse); err != nil {
		t.Fatalf("decode JRE preflight AppCall response: %v", err)
	}
	var preflight map[string]any
	if err := json.Unmarshal(preflightResponse.Body, &preflight); err != nil {
		t.Fatalf("decode JRE preflight status %d body %s: %v", preflightResponse.Status, preflightResponse.Body, err)
	}
	if preflightResponse.Status != http.StatusOK ||
		fmt.Sprint(preflight["engine"]) != "runtime-proof" ||
		preflight["compatible"] != true {
		t.Fatalf("Resolver -> JRE sibling-provider preflight = %d %s", preflightResponse.Status, preflightResponse.Body)
	}

	callsMu.Lock()
	gotCalls := append([]pulpThreeAppResolverCall20260725(nil), calls...)
	callsMu.Unlock()
	wantHealth := pulpThreeAppResolverCall20260725{
		Cell: "minecraft-resolver", Provider: pulpThreeAppResolverProvider20260725,
		Method: http.MethodGet, Path: "/health",
	}
	wantPreflight := pulpThreeAppResolverCall20260725{
		Cell: "minecraft-resolver", Provider: pulpThreeAppResolverProvider20260725,
		Method: http.MethodPost, Path: "/preflight/jre",
	}
	var healthCalls, preflightCalls []pulpThreeAppResolverCall20260725
	for _, call := range gotCalls {
		if call.Method == http.MethodGet && call.Path == "/health" {
			healthCalls = append(healthCalls, call)
		}
		if call.Method == http.MethodPost && call.Path == "/preflight/jre" {
			preflightCalls = append(preflightCalls, call)
		}
	}
	if len(healthCalls) != 1 || healthCalls[0] != wantHealth ||
		len(preflightCalls) != 1 || preflightCalls[0] != wantPreflight {
		t.Fatalf("Resolver AppCalls = %#v, want exactly one %#v and one %#v",
			gotCalls, wantHealth, wantPreflight)
	}
}

func assertPulpThreeAppResolverHostShape20260725(t *testing.T, hostManifest *manifest.Host) {
	t.Helper()
	if len(hostManifest.Applications) != 3 || len(hostManifest.Routes) != 1 {
		t.Fatalf("production host = %d applications, %d routes; want 3 and 1",
			len(hostManifest.Applications), len(hostManifest.Routes))
	}
	route := hostManifest.Routes[0]
	if route.Path != "/" || route.Application != "evolution" || route.Instance != "primary" {
		t.Fatalf("production public host route = %#v, want Evolution only", route)
	}
	for _, binding := range hostManifest.Routes {
		if binding.Application == "minecraft-resolver" {
			t.Fatalf("Resolver has a forbidden standalone host route: %#v", binding)
		}
	}

	var resolver, evolution *manifest.HostedApplication
	for _, application := range hostManifest.Applications {
		if application.ID == "minecraft-resolver" {
			resolver = application
		}
		if application.ID == "evolution" {
			evolution = application
		}
	}
	if resolver == nil || resolver.Application == nil {
		t.Fatal("production host has no Resolver application")
	}
	var resolverCell, jreCell *manifest.CellSpec
	for _, spec := range resolver.Application.Cells.Order {
		switch spec.Name {
		case "minecraft-resolver":
			resolverCell = spec
		case "jvm-jre-detect":
			jreCell = spec
		}
		for _, capability := range spec.Capabilities {
			if capability == "transport.http.inbound" {
				t.Fatalf("Resolver provider cell %s retains forbidden standalone inbound HTTP capability", spec.Name)
			}
		}
	}
	if resolverCell == nil || jreCell == nil ||
		len(resolverCell.Consumes) != 1 || resolverCell.Consumes[0] != "jvm-jre-detect.route.v1" ||
		len(jreCell.Provides) != 1 || jreCell.Provides[0] != "jvm-jre-detect.route.v1" {
		t.Fatalf("Resolver -> JRE provider contract = resolver consumes %#v, JRE provides %#v",
			resolverCell, jreCell)
	}
	if evolution == nil || evolution.Application == nil {
		t.Fatal("production host has no Evolution application")
	}
	for _, spec := range evolution.Application.Cells.Order {
		for key := range spec.Config {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "minecraft_resolver_url" ||
				normalized == "minecraft_sidecar_url" ||
				normalized == "resolver_url" {
				t.Fatalf("Evolution cell %s retains standalone Resolver config %q", spec.Name, key)
			}
		}
	}
}
