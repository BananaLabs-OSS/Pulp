package run

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/fusion"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func automaticFusionFixture() (*manifest.Application, fusion.Plan) {
	a := &manifest.CellSpec{Name: "a", Version: "1.0.0", Provides: []string{"a.v1"}, Execution: manifest.ExecutionSpec{Mode: manifest.ExecutionFusible, Group: "engine", ABI: "register-v1"}}
	b := &manifest.CellSpec{Name: "b", Version: "1.0.0", Provides: []string{"b.v1"}, Execution: manifest.ExecutionSpec{Mode: manifest.ExecutionFusible, Group: "engine", ABI: "register-v1"}}
	app := &manifest.Application{Cells: &manifest.Set{Cells: []*manifest.CellSpec{a, b}, Order: []*manifest.CellSpec{a, b}}, Placements: []manifest.CellPlacement{{Spec: a, InstanceID: "primary", Address: "a"}, {Spec: b, InstanceID: "primary", Address: "b"}}}
	return app, fusion.Build(app.Cells.Order)
}

func TestPrepareAutomaticFusionAddsPhysicalExecutionUnit(t *testing.T) {
	app, plan := automaticFusionFixture()
	app.Cells.Order[0].Config = map[string]any{"mode": "same"}
	app.Cells.Order[1].Config = map[string]any{"mode": "same"}
	app.Cells.Order[0].Restart, app.Cells.Order[1].Restart = "always", "always"
	app.Cells.Order[0].CallTimeoutMS, app.Cells.Order[1].CallTimeoutMS = 25, 25
	artifact := &manifest.CellSpec{Name: "engine-fused", Provides: []string{"a.v1", "b.v1"}, Execution: manifest.ExecutionSpec{Mode: manifest.ExecutionFusible, Group: "engine", ABI: "register-v1"}}
	called := 0
	prepared, fallbacks, err := prepareAutomaticFusion(context.Background(), ApplicationIdentity{ApplicationID: "game", InstanceID: "one"}, app, plan, FusionRuntimePreparerFunc(func(_ context.Context, identity ApplicationIdentity, group fusion.Group) (*fusion.Activation, error) {
		called++
		if identity.ApplicationID != "game" || group.Name != "engine" {
			t.Fatalf("unexpected request: %#v %#v", identity, group)
		}
		return &fusion.Activation{Fused: true, Spec: artifact}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || len(fallbacks) != 0 || len(prepared.ExecutionUnits) != 1 {
		t.Fatalf("called=%d fallbacks=%v units=%#v", called, fallbacks, prepared.ExecutionUnits)
	}
	unit := prepared.ExecutionUnits[0]
	if unit.Artifact == artifact || unit.Artifact.Name != artifact.Name || !reflect.DeepEqual(unit.Members, []string{"a", "b"}) {
		t.Fatalf("unit = %#v", unit)
	}
	if !reflect.DeepEqual(unit.Artifact.Config, map[string]any{"mode": "same"}) || unit.Artifact.Restart != "always" || unit.Artifact.CallTimeoutMS != 25 {
		t.Fatalf("physical runtime policy = %#v", unit.Artifact)
	}
	if len(app.ExecutionUnits) != 0 {
		t.Fatal("source application was mutated")
	}
}

func TestPrepareAutomaticFusionFallbackMustBeExplicit(t *testing.T) {
	app, plan := automaticFusionFixture()
	cause := errors.New("registry offline")
	prepared, fallbacks, err := prepareAutomaticFusion(context.Background(), ApplicationIdentity{}, app, plan, FusionRuntimePreparerFunc(func(context.Context, ApplicationIdentity, fusion.Group) (*fusion.Activation, error) {
		return &fusion.Activation{Fallback: cause}, nil
	}))
	if err != nil || prepared == nil || len(prepared.ExecutionUnits) != 0 || len(fallbacks) != 1 || !errors.Is(fallbacks[0], cause) {
		t.Fatalf("prepared=%p app=%p fallbacks=%v err=%v", prepared, app, fallbacks, err)
	}
	_, _, err = prepareAutomaticFusion(context.Background(), ApplicationIdentity{}, app, plan, FusionRuntimePreparerFunc(func(context.Context, ApplicationIdentity, fusion.Group) (*fusion.Activation, error) {
		return nil, cause
	}))
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("required fusion error = %v", err)
	}
	_, _, err = prepareAutomaticFusion(context.Background(), ApplicationIdentity{}, app, plan, FusionRuntimePreparerFunc(func(context.Context, ApplicationIdentity, fusion.Group) (*fusion.Activation, error) {
		return &fusion.Activation{}, nil
	}))
	if err == nil || !strings.Contains(err.Error(), "without a cause") {
		t.Fatalf("implicit fallback error = %v", err)
	}
}

func TestPrepareAutomaticFusionRejectsPartialManualOwnership(t *testing.T) {
	app, plan := automaticFusionFixture()
	app.ExecutionUnits = []manifest.ExecutionUnit{{Name: "manual", Artifact: &manifest.CellSpec{Name: "manual"}, Members: []string{"a"}}}
	_, _, err := prepareAutomaticFusion(context.Background(), ApplicationIdentity{}, app, plan, FusionRuntimePreparerFunc(func(context.Context, ApplicationIdentity, fusion.Group) (*fusion.Activation, error) {
		t.Fatal("preparer should not run")
		return nil, nil
	}))
	if err == nil || !strings.Contains(err.Error(), "partially covered") {
		t.Fatalf("error = %v", err)
	}
}

func TestCoordinatorFusionPreparerRequiresCompleteConfiguration(t *testing.T) {
	var empty CoordinatorFusionPreparer
	if _, err := empty.PrepareFusion(context.Background(), ApplicationIdentity{}, fusion.Group{}); err == nil || !strings.Contains(err.Error(), "coordinator") {
		t.Fatalf("missing coordinator error = %v", err)
	}
	empty.Coordinator = &fusion.Coordinator{}
	if _, err := empty.PrepareFusion(context.Background(), ApplicationIdentity{}, fusion.Group{}); err == nil || !strings.Contains(err.Error(), "request factory") {
		t.Fatalf("missing request error = %v", err)
	}
}
