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

func cell(name string, caps []string) *manifest.CellSpec {
	return &manifest.CellSpec{Name: name, Capabilities: caps, Restart: manifest.RestartOnCrash, Execution: manifest.ExecutionSpec{Mode: manifest.ExecutionFusible, Group: "state", ABI: "pulp-linear-v1"}}
}
