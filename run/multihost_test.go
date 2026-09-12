package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMultiHostSupervisorUsesCanonicalStartupAndReverseShutdownOrder(t *testing.T) {
	var lifecycle []string
	loader := HostManifestLoaderFunc(func(context.Context, string) ([]HostedApplication, error) {
		return []HostedApplication{
			testHostedApplication("sessions", "b2"),
			testHostedApplication("evolution", "a"),
			testHostedApplication("sessions", "b1"),
		}, nil
	})
	factory := ApplicationRuntimeFactoryFunc(func(_ context.Context, app HostedApplication) (ApplicationRuntime, error) {
		return &fakeApplicationRuntime{identity: app.Identity, lifecycle: &lifecycle}, nil
	})
	supervisor := testMultiHostSupervisor(t, loader, factory)

	if err := supervisor.Start(context.Background(), "pulp.host.toml"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := []string{
		"start:evolution/a", "start:sessions/b1", "start:sessions/b2",
		"shutdown:sessions/b2", "shutdown:sessions/b1", "shutdown:evolution/a",
	}
	if diff := lifecycleDiff(want, lifecycle); diff != "" {
		t.Fatal(diff)
	}
}

func TestMultiHostSupervisorRollsBackFailingAndStartedRuntimes(t *testing.T) {
	var lifecycle []string
	loader := HostManifestLoaderFunc(func(context.Context, string) ([]HostedApplication, error) {
		return []HostedApplication{
			testHostedApplication("alpha", "one"),
			testHostedApplication("bravo", "one"),
			testHostedApplication("charlie", "one"),
		}, nil
	})
	factory := ApplicationRuntimeFactoryFunc(func(_ context.Context, app HostedApplication) (ApplicationRuntime, error) {
		runtime := &fakeApplicationRuntime{identity: app.Identity, lifecycle: &lifecycle}
		if app.Identity.ApplicationID == "bravo" {
			runtime.startErr = errors.New("init failed")
		}
		return runtime, nil
	})
	supervisor := testMultiHostSupervisor(t, loader, factory)

	err := supervisor.Start(context.Background(), "pulp.host.toml")
	if err == nil || err.Error() != "start application bravo/one: init failed" {
		t.Fatalf("Start error = %v, want bravo failure", err)
	}
	want := []string{
		"start:alpha/one", "start:bravo/one",
		"shutdown:bravo/one", "shutdown:alpha/one",
	}
	if diff := lifecycleDiff(want, lifecycle); diff != "" {
		t.Fatal(diff)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after failed start: %v", err)
	}
}

func TestMultiHostSupervisorStartsApplicationDependenciesFirst(t *testing.T) {
	var lifecycle []string
	loader := HostManifestLoaderFunc(func(context.Context, string) ([]HostedApplication, error) {
		control := testHostedApplication("control", "primary")
		commerce := testHostedApplication("commerce", "primary")
		commerce.DependsOn = []string{"control"}
		return []HostedApplication{commerce, control}, nil
	})
	factory := ApplicationRuntimeFactoryFunc(func(_ context.Context, app HostedApplication) (ApplicationRuntime, error) {
		return &fakeApplicationRuntime{identity: app.Identity, lifecycle: &lifecycle}, nil
	})
	supervisor := testMultiHostSupervisor(t, loader, factory)

	if err := supervisor.Start(context.Background(), "pulp.host.toml"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := []string{
		"start:control/primary", "start:commerce/primary",
		"shutdown:commerce/primary", "shutdown:control/primary",
	}
	if diff := lifecycleDiff(want, lifecycle); diff != "" {
		t.Fatal(diff)
	}
}

func TestMultiHostSupervisorRejectsDuplicateIdentityBeforeCreatingRuntimes(t *testing.T) {
	loader := HostManifestLoaderFunc(func(context.Context, string) ([]HostedApplication, error) {
		return []HostedApplication{testHostedApplication("sessions", "primary"), testHostedApplication("sessions", "primary")}, nil
	})
	factoryCalls := 0
	factory := ApplicationRuntimeFactoryFunc(func(_ context.Context, app HostedApplication) (ApplicationRuntime, error) {
		factoryCalls++
		return &fakeApplicationRuntime{identity: app.Identity}, nil
	})
	supervisor := testMultiHostSupervisor(t, loader, factory)

	err := supervisor.Start(context.Background(), "pulp.host.toml")
	if err == nil || err.Error() != "duplicate hosted application sessions/primary" {
		t.Fatalf("Start error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want 0", factoryCalls)
	}
}

func TestHostedApplicationCellScopeSeparatesEqualCellNames(t *testing.T) {
	left := testHostedApplication("sessions", "primary")
	right := testHostedApplication("sessions", "secondary")
	leftScope, err := left.NewCellScope("lua", "primary")
	if err != nil {
		t.Fatalf("left NewCellScope: %v", err)
	}
	rightScope, err := right.NewCellScope("lua", "primary")
	if err != nil {
		t.Fatalf("right NewCellScope: %v", err)
	}
	if leftScope.RoutingID() == rightScope.RoutingID() {
		t.Fatalf("routing IDs unexpectedly collide: %q", leftScope.RoutingID())
	}
	if leftScope.ApplicationID() != "sessions" || leftScope.ApplicationInstanceID() != "primary" {
		t.Fatalf("left scope = %#v", leftScope)
	}
}

func TestManifestHostLoaderExpandsEveryDeclaredInstance(t *testing.T) {
	root := t.TempDir()
	writeMultiHostTestFile(t, root, "lua.cell.toml", "name = \"lua\"\nversion = \"1\"\n")
	writeMultiHostTestFile(t, root, "app.lua", "return {}\n")
	writeMultiHostTestFile(t, root, "sessions.app.toml", `
name = "sessions"
version = "1"
cells = ["lua.cell.toml"]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "1232d8379de77e154ca533689af2e42629dd7574bda5a0a390799849f07607c3"
`)
	hostPath := writeMultiHostTestFile(t, root, "pulp.host.toml", `
name = "test-host"
[[applications]]
id = "sessions"
manifest = "sessions.app.toml"
instances = 2
aliases = ["primary", "secondary"]
storage_namespace = "sessions-storage"
event_namespace = "sessions-events"
`)

	applications, err := (ManifestHostLoader{}).LoadHostApplications(context.Background(), hostPath)
	if err != nil {
		t.Fatalf("LoadHostApplications: %v", err)
	}
	if len(applications) != 2 {
		t.Fatalf("applications = %d, want 2", len(applications))
	}
	if applications[0].Identity.String() != "sessions/primary" || applications[1].Identity.String() != "sessions/secondary" {
		t.Fatalf("identities = %#v", applications)
	}
	if applications[0].StorageNamespace != "sessions-storage" || applications[1].EventNamespace != "sessions-events" {
		t.Fatalf("applications = %#v", applications)
	}
}

func TestMultiHostSupervisorSerializesConcurrentLifecycleCalls(t *testing.T) {
	loader := HostManifestLoaderFunc(func(context.Context, string) ([]HostedApplication, error) {
		return []HostedApplication{testHostedApplication("sessions", "primary")}, nil
	})
	enteredStart := make(chan struct{})
	releaseStart := make(chan struct{})
	runtime := &fakeApplicationRuntime{
		identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"},
		startHook: func() {
			close(enteredStart)
			<-releaseStart
		},
	}
	factory := ApplicationRuntimeFactoryFunc(func(context.Context, HostedApplication) (ApplicationRuntime, error) {
		return runtime, nil
	})
	supervisor := testMultiHostSupervisor(t, loader, factory)

	first := make(chan error, 1)
	go func() { first <- supervisor.Start(context.Background(), "pulp.host.toml") }()
	<-enteredStart
	second := make(chan error, 1)
	go func() { second <- supervisor.Start(context.Background(), "pulp.host.toml") }()
	close(releaseStart)
	if err := <-first; err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := <-second; !errors.Is(err, ErrMultiHostRunning) {
		t.Fatalf("second Start: %v, want ErrMultiHostRunning", err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func testMultiHostSupervisor(t *testing.T, loader HostManifestLoader, factory ApplicationRuntimeFactory) *MultiHostSupervisor {
	t.Helper()
	supervisor, err := NewMultiHostSupervisor(loader, factory)
	if err != nil {
		t.Fatalf("NewMultiHostSupervisor: %v", err)
	}
	return supervisor
}

func testHostedApplication(applicationID, instanceID string) HostedApplication {
	return HostedApplication{
		Identity:     ApplicationIdentity{ApplicationID: applicationID, InstanceID: instanceID},
		ManifestPath: applicationID + ".app.toml",
	}
}

type fakeApplicationRuntime struct {
	identity  ApplicationIdentity
	lifecycle *[]string
	startErr  error
	stopErr   error
	startHook func()

	mu sync.Mutex
}

func (r *fakeApplicationRuntime) Identity() ApplicationIdentity { return r.identity }

func (r *fakeApplicationRuntime) Start(context.Context) error {
	if r.startHook != nil {
		r.startHook()
	}
	r.record("start")
	return r.startErr
}

func (r *fakeApplicationRuntime) Shutdown(context.Context) error {
	r.record("shutdown")
	return r.stopErr
}

func (r *fakeApplicationRuntime) record(operation string) {
	if r.lifecycle == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.lifecycle = append(*r.lifecycle, fmt.Sprintf("%s:%s", operation, r.identity))
}

func lifecycleDiff(want, got []string) string {
	if len(want) != len(got) {
		return fmt.Sprintf("lifecycle length = %d, want %d; got %#v", len(got), len(want), got)
	}
	for index := range want {
		if want[index] != got[index] {
			return fmt.Sprintf("lifecycle[%d] = %q, want %q; got %#v", index, got[index], want[index], got)
		}
	}
	return ""
}

func writeMultiHostTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
