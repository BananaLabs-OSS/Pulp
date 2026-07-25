package run

import (
	"context"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestValidateRuntimeInputs(t *testing.T) {
	tests := []struct {
		name      string
		hostPath  string
		appPath   string
		manifests []string
		wantErr   bool
	}{
		{name: "host", hostPath: "pulp.host.toml"},
		{name: "app", appPath: "pulp.app.toml"},
		{name: "manifest", manifests: []string{"pulp.cell.toml"}},
		{name: "missing", wantErr: true},
		{name: "host and app", hostPath: "pulp.host.toml", appPath: "pulp.app.toml", wantErr: true},
		{name: "host and manifest", hostPath: "pulp.host.toml", manifests: []string{"pulp.cell.toml"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRuntimeInputs(test.hostPath, test.appPath, test.manifests)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateRuntimeInputs() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestHostedApplicationHostStopsGatewayBeforeApplications(t *testing.T) {
	runtime := &hostCLIHTTPRuntime{
		identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"},
		address:  "127.0.0.1:1",
	}
	supervisor := &MultiHostSupervisor{
		state:    multiHostRunning,
		runtimes: []ApplicationRuntime{runtime},
	}
	gateway, err := NewSupervisorHostGateway("127.0.0.1:0", &manifest.Host{Routes: []*manifest.RouteBinding{{
		Path:        "/sessions",
		Application: "sessions",
		Instance:    "primary",
	}}}, supervisor, nil)
	if err != nil {
		t.Fatalf("NewSupervisorHostGateway: %v", err)
	}
	if err := gateway.Start(context.Background()); err != nil {
		t.Fatalf("gateway Start: %v", err)
	}
	hosted := &hostedApplicationHost{supervisor: supervisor, gateway: gateway}
	if err := hosted.Shutdown(context.Background()); err != nil {
		t.Fatalf("hosted Shutdown: %v", err)
	}
	if runtime.stops != 1 {
		t.Fatalf("application shutdown calls = %d, want 1", runtime.stops)
	}
	if err := gateway.Start(context.Background()); err == nil {
		t.Fatal("gateway restarted after hosted shutdown")
	}
}

func TestHostGatewayAddress(t *testing.T) {
	for _, test := range []struct {
		requested string
		want      string
		wantErr   bool
	}{
		{requested: "8080", want: ":8080"},
		{requested: "127.0.0.1:8080", want: "127.0.0.1:8080"},
		{wantErr: true},
	} {
		got, err := hostGatewayAddress(test.requested)
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatalf("hostGatewayAddress(%q) = (%q, %v), want (%q, error=%v)", test.requested, got, err, test.want, test.wantErr)
		}
	}
}

func TestStartHostedApplicationsPassesHostOptionsAndStops(t *testing.T) {
	runtime := &hostCLIRuntime{identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}}
	factory := ApplicationRuntimeFactoryFunc(func(context.Context, HostedApplication) (ApplicationRuntime, error) {
		return runtime, nil
	})

	loader := HostManifestLoaderFunc(func(context.Context, string) ([]HostedApplication, error) {
		return []HostedApplication{{
			Identity:     runtime.identity,
			ManifestPath: "sessions/pulp.app.toml",
		}}, nil
	})
	supervisor, err := startHostedApplicationsWithFactory(context.Background(), "pulp.host.toml", loader, factory)
	if err != nil {
		t.Fatalf("startHostedApplicationsWithFactory: %v", err)
	}
	if runtime.starts != 1 {
		t.Fatalf("runtime starts = %d, want 1", runtime.starts)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if runtime.stops != 1 {
		t.Fatalf("runtime stops = %d, want 1", runtime.stops)
	}
}

type hostCLIRuntime struct {
	identity ApplicationIdentity
	starts   int
	stops    int
}

func (r *hostCLIRuntime) Identity() ApplicationIdentity { return r.identity }

func (r *hostCLIRuntime) Start(context.Context) error {
	r.starts++
	return nil
}

func (r *hostCLIRuntime) Shutdown(context.Context) error {
	r.stops++
	return nil
}

type hostCLIHTTPRuntime struct {
	identity ApplicationIdentity
	address  string
	stops    int
}

func (r *hostCLIHTTPRuntime) Identity() ApplicationIdentity { return r.identity }
func (r *hostCLIHTTPRuntime) Start(context.Context) error   { return nil }
func (r *hostCLIHTTPRuntime) Shutdown(context.Context) error {
	r.stops++
	return nil
}
func (r *hostCLIHTTPRuntime) HTTPAddress() string { return r.address }
