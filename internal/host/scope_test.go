package host

import (
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

func TestCellExposesAssignedScopeToExtensions(t *testing.T) {
	scope, err := ext.NewScope("sessions", "primary", "gene", "default")
	if err != nil {
		t.Fatal(err)
	}
	cell := &Cell{name: "gene", scope: scope}
	if got := ext.ScopeOf(cell); got != scope {
		t.Fatalf("ScopeOf(cell) = %#v, want %#v", got, scope)
	}
	if got, want := ext.CellIDOf(cell), scope.RoutingID(); got != want {
		t.Fatalf("CellIDOf(cell) = %q, want %q", got, want)
	}
}

func TestLegacyLoadedCellKeepsNameEventTarget(t *testing.T) {
	cell := &Cell{name: "gene", scope: ext.LegacyScope("gene")}
	if got, want := ext.CellIDOf(cell), "gene"; got != want {
		t.Fatalf("CellIDOf(cell) = %q, want %q", got, want)
	}
}
