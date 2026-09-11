package run

import (
	"reflect"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestPhysicalDependencyPlanExpandsRepeatedPlacements(t *testing.T) {
	a, b := &manifest.CellSpec{Name: "a"}, &manifest.CellSpec{Name: "b", DependsOn: []string{"a"}}
	logical, _ := dependency.Build([]dependency.Item{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}})
	app := &manifest.Application{Cells: &manifest.Set{Cells: []*manifest.CellSpec{a, b}, Plan: logical}}
	placements := []manifest.CellPlacement{{Spec: a, Address: "a@1"}, {Spec: a, Address: "a@2"}, {Spec: b, Address: "b"}}
	plan, err := physicalDependencyPlan(app, placements, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := plan.Node("b")
	if got, want := node.Dependencies(), []string{"a@1", "a@2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependencies = %#v, want %#v", got, want)
	}
}

func TestPhysicalDependencyPlanCollapsesFusedMembers(t *testing.T) {
	a, b, c := &manifest.CellSpec{Name: "a"}, &manifest.CellSpec{Name: "b", DependsOn: []string{"a"}}, &manifest.CellSpec{Name: "c", DependsOn: []string{"b"}}
	logical, _ := dependency.Build([]dependency.Item{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}, {ID: "c", DependsOn: []string{"b"}}})
	artifact := &manifest.CellSpec{Name: "ab"}
	app := &manifest.Application{Cells: &manifest.Set{Cells: []*manifest.CellSpec{a, b, c}, Plan: logical}}
	plan, err := physicalDependencyPlan(app, []manifest.CellPlacement{{Spec: artifact, Address: "ab"}, {Spec: c, Address: "c"}}, map[string]string{"a": "ab", "b": "ab"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Levels(), [][]string{{"ab"}, {"c"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("levels = %#v, want %#v", got, want)
	}
}
