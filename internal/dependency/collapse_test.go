package dependency

import (
	"reflect"
	"strings"
	"testing"
)

func TestCollapseRemovesInternalEdgesAndDeduplicatesExternalEdges(t *testing.T) {
	logical, _ := Build([]Item{
		{ID: "database"},
		{ID: "model", DependsOn: []string{"database"}},
		{ID: "api", DependsOn: []string{"database", "model"}},
		{ID: "web", DependsOn: []string{"api"}},
	})
	physical, err := Collapse(logical, map[string]string{
		"database": "core.wasm", "model": "core.wasm",
		"api": "service.wasm", "web": "web.wasm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := physical.Levels(), [][]string{{"core.wasm"}, {"service.wasm"}, {"web.wasm"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Levels = %#v, want %#v", got, want)
	}
	node, _ := physical.Node("service.wasm")
	if got, want := node.Dependencies(), []string{"core.wasm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependencies = %#v, want %#v", got, want)
	}
}

func TestCollapseRequiresEveryMapping(t *testing.T) {
	logical, _ := Build([]Item{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}})
	_, err := Collapse(logical, map[string]string{"a": "unit"})
	if err == nil || !strings.Contains(err.Error(), `"b" has no physical target`) {
		t.Fatalf("error = %v", err)
	}
}
