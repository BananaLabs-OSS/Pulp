package dependency

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildDiamondPlan(t *testing.T) {
	plan, err := Build([]Item{
		{ID: "root"},
		{ID: "left", DependsOn: []string{"root"}},
		{ID: "right", DependsOn: []string{"root"}},
		{ID: "leaf", DependsOn: []string{"left", "right"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Levels(), [][]string{{"root"}, {"left", "right"}, {"leaf"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Levels = %#v, want %#v", got, want)
	}
	if got, want := plan.StartOrder(), []string{"root", "left", "right", "leaf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StartOrder = %#v, want %#v", got, want)
	}
	if got, want := plan.StopOrder(), []string{"leaf", "right", "left", "root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StopOrder = %#v, want %#v", got, want)
	}
	node, ok := plan.Node("root")
	if !ok || !reflect.DeepEqual(node.Dependents(), []string{"left", "right"}) {
		t.Fatalf("root = %#v, %v", node, ok)
	}
	levels := plan.Levels()
	levels[0][0] = "mutated"
	if plan.Levels()[0][0] != "root" {
		t.Fatal("Levels exposed mutable plan storage")
	}
}

func TestBuildRejectsInvalidGraphs(t *testing.T) {
	tests := []struct {
		name  string
		items []Item
		want  string
	}{
		{"empty", nil, "at least one"},
		{"duplicate", []Item{{ID: "a"}, {ID: "a"}}, "duplicate"},
		{"missing", []Item{{ID: "a", DependsOn: []string{"b"}}}, "unknown"},
		{"self", []Item{{ID: "a", DependsOn: []string{"a"}}}, "itself"},
		{"duplicate edge", []Item{{ID: "a"}, {ID: "b", DependsOn: []string{"a", "a"}}}, "more than once"},
		{"cycle", []Item{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}, {ID: "free"}}, "dependency cycle involving: a, b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Build(test.items)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}
