package ext

import "testing"

func grantScope(t *testing.T, id string) Scope {
	t.Helper()
	s, e := NewScope("projx", "desktop", "files", id)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestStaticPlacementGrantsExactScopeAndImmutableCopies(t *testing.T) {
	s := grantScope(t, "one")
	r, e := NewStaticPlacementGrants([]PlacementGrant{{Scope: s, Capability: " STORAGE.FS ", Resource: "source", Rights: []string{"stat", "read"}, Attributes: map[string]string{"root": "/chosen"}}})
	if e != nil {
		t.Fatal(e)
	}
	g, ok := r.ResolvePlacementGrant(s, "storage.fs")
	if !ok || !g.Allows("READ") || g.Allows("write") {
		t.Fatal(g, ok)
	}
	g.Rights[0] = "write"
	g.Attributes["root"] = "/other"
	again, _ := r.ResolvePlacementGrant(s, "storage.fs")
	if again.Allows("write") || again.Attributes["root"] != "/chosen" {
		t.Fatal("resolver leaked mutable state")
	}
	if _, ok := r.ResolvePlacementGrant(grantScope(t, "two"), "storage.fs"); ok {
		t.Fatal("grant crossed placement")
	}
}
func TestStaticPlacementGrantsRejectInvalidAndDuplicate(t *testing.T) {
	s := grantScope(t, "one")
	cases := [][]PlacementGrant{{{Scope: Scope{}, Capability: "x", Resource: "r", Rights: []string{"read"}}}, {{Scope: s, Capability: "", Resource: "r", Rights: []string{"read"}}}, {{Scope: s, Capability: "x", Resource: "r"}}, {{Scope: s, Capability: "x", Resource: "r", Rights: []string{"read", "READ"}}}, {{Scope: s, Capability: "x", Resource: "r", Rights: []string{"bad right"}}}}
	for _, c := range cases {
		if _, e := NewStaticPlacementGrants(c); e == nil {
			t.Fatalf("accepted %#v", c)
		}
	}
	g := PlacementGrant{Scope: s, Capability: "x", Resource: "r", Rights: []string{"read"}}
	if _, e := NewStaticPlacementGrants([]PlacementGrant{g, g}); e == nil {
		t.Fatal("accepted duplicate scope/capability")
	}
}
