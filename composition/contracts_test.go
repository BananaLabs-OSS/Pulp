package composition

import "testing"

func TestVersionedContractsResolveOnlyThroughOwner(t *testing.T) {
	catalog, err := Build([]Contract{{ID: "movement.intent.v1", Kind: Command, Owner: "movement", Request: "movement.Intent.v1"}}, []Module{{Name: "movement", Provides: []string{"movement.intent.v1"}}, {Name: "game", Consumes: []string{"movement.intent.v1"}}, {Name: "lua"}})
	if err != nil {
		t.Fatal(err)
	}
	contract, owner, err := catalog.Resolve("lua", "movement.intent.v1")
	if err != nil || owner != "movement" || contract.Kind != Command {
		t.Fatalf("resolve = %#v, %q, %v", contract, owner, err)
	}
	if _, _, err = catalog.Resolve("lua", "movement.intent.v2"); err == nil {
		t.Fatal("undeclared contract resolved")
	}
	if _, _, err = catalog.Resolve("movement", "movement.intent.v1"); err == nil {
		t.Fatal("owner routed through composition")
	}
}

func TestContractsRejectAmbiguousOrUnversionedMeaning(t *testing.T) {
	modules := []Module{{Name: "a", Provides: []string{"thing.v1"}}, {Name: "b"}}
	if _, err := Build([]Contract{{ID: "thing", Kind: Query, Owner: "a"}}, modules); err == nil {
		t.Fatal("unversioned contract accepted")
	}
	if _, err := Build([]Contract{{ID: "thing.v1", Kind: Query, Owner: "a"}, {ID: "thing.v1", Kind: Query, Owner: "b"}}, modules); err == nil {
		t.Fatal("duplicate contract accepted")
	}
}
