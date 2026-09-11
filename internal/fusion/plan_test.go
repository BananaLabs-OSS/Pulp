package fusion

import (
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestBuildAcceptsCompatibleGroup(t *testing.T) {
	plan := Build([]*manifest.CellSpec{cell("registry", nil), cell("catalog", nil)})
	if len(plan.Groups) != 1 || plan.Groups[0].Name != "state" || len(plan.Groups[0].Members) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestBuildFallsBackWhenCapabilityAuthorityWouldBroaden(t *testing.T) {
	plan := Build([]*manifest.CellSpec{cell("registry", nil), cell("worker", []string{"workers"})})
	if len(plan.Groups) != 0 || len(plan.Isolated) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	if got := plan.Isolated[0].Reason; got != "capability set differs within fusion group" {
		t.Fatalf("reason = %q", got)
	}
}

func TestBuildFallsBackForOneMember(t *testing.T) {
	plan := Build([]*manifest.CellSpec{cell("only", nil)})
	if len(plan.Groups) != 0 || len(plan.Isolated) != 1 || plan.Isolated[0].Reason != "fusion group has fewer than two members" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestBuildFallsBackWhenV1CannotPreserveLogicalRuntimeBoundary(t *testing.T) {
	t.Run("different config", func(t *testing.T) {
		a, b := cell("a", nil), cell("b", nil)
		a.Config, b.Config = map[string]any{"mode": "a"}, map[string]any{"mode": "b"}
		plan := Build([]*manifest.CellSpec{a, b})
		if len(plan.Groups) != 0 || plan.Isolated[0].Reason != "config differs within fusion group" {
			t.Fatalf("plan = %#v", plan)
		}
	})
	t.Run("snapshot", func(t *testing.T) {
		a, b := cell("a", nil), cell("b", nil)
		a.Snapshotable = true
		plan := Build([]*manifest.CellSpec{a, b})
		if len(plan.Groups) != 0 || plan.Isolated[0].Reason != "snapshotable cells require per-member snapshot ABI support" {
			t.Fatalf("plan = %#v", plan)
		}
	})
}

func TestBuildV2AllowsMemberConfigAndSnapshotPartitions(t *testing.T) {
	a, b := cell("a", []string{"storage.sqlite"}), cell("b", []string{"storage.sqlite"})
	a.Execution.ABI, b.Execution.ABI = "pulp-member-v2", "pulp-member-v2"
	a.Config, b.Config = map[string]any{"member": "a"}, map[string]any{"member": "b"}
	a.Snapshotable, b.Snapshotable = true, true
	plan := Build([]*manifest.CellSpec{a, b})
	if len(plan.Groups) != 1 || len(plan.Isolated) != 0 {
		t.Fatalf("v2 plan = %+v", plan)
	}
}

func cell(name string, caps []string) *manifest.CellSpec {
	return &manifest.CellSpec{Name: name, Capabilities: caps, Restart: manifest.RestartOnCrash, Execution: manifest.ExecutionSpec{Mode: manifest.ExecutionFusible, Group: "state", ABI: "pulp-linear-v1"}}
}
