package run

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestCellRuntimeRoutingAndTeardownTargetsUseExplicitScope(t *testing.T) {
	scope, err := ext.NewScope("sessions", "primary", "gene", "worker-2")
	if err != nil {
		t.Fatal(err)
	}
	rt := &cellRuntime{
		spec:        &manifest.CellSpec{Name: "gene"},
		scope:       scope,
		eventTarget: scope.RoutingID(),
	}
	if got, want := rt.eventTargetID(), scope.RoutingID(); got != want {
		t.Fatalf("eventTargetID = %q, want %q", got, want)
	}
	if got, want := rt.effectiveScope(), scope; got != want {
		t.Fatalf("effectiveScope = %#v, want %#v", got, want)
	}
}

func TestCellRuntimeLegacyFallbackUsesCellName(t *testing.T) {
	rt := &cellRuntime{spec: &manifest.CellSpec{Name: "gene"}}
	if got, want := rt.eventTargetID(), "gene"; got != want {
		t.Fatalf("eventTargetID = %q, want %q", got, want)
	}
	if got, want := rt.effectiveScope(), ext.LegacyScope("gene"); got != want {
		t.Fatalf("effectiveScope = %#v, want %#v", got, want)
	}
}

func TestRuntimeOpsTeardownUsesScopedTargetAndLegacyName(t *testing.T) {
	for _, test := range []struct {
		name   string
		scope  ext.Scope
		target string
	}{
		{
			name:   "scoped",
			scope:  mustScope(t, "sessions", "primary", "gene", "default"),
			target: mustScope(t, "sessions", "primary", "gene", "default").RoutingID(),
		},
		{
			name:   "legacy",
			scope:  ext.LegacyScope("gene"),
			target: "gene",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			got := ""
			rt := &cellRuntime{
				spec:     &manifest.CellSpec{Name: "gene"},
				scope:    test.scope,
				events:   make(chan routedEvent),
				ctx:      ctx,
				cancel:   cancel,
				declared: map[string]bool{"test.capability": true},
			}
			ops := &runtimeOps{
				runtimes: map[string]*cellRuntime{"gene": rt},
				allCaps: []ext.Capability{{
					Name: "test.capability",
					TeardownCell: func(_ context.Context, cellID string) error {
						got = cellID
						return nil
					},
				}},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			if err := ops.shutdownCell("gene"); err != nil {
				t.Fatal(err)
			}
			if got != test.target {
				t.Fatalf("TeardownCell target = %q, want %q", got, test.target)
			}
		})
	}
}

func mustScope(t *testing.T, applicationID, applicationInstanceID, cellID, cellInstanceID string) ext.Scope {
	t.Helper()
	scope, err := ext.NewScope(applicationID, applicationInstanceID, cellID, cellInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
