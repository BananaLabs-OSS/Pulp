package ext

import (
	"strings"
	"testing"
)

type scopedTestCell struct {
	name  string
	scope Scope
}

func (c scopedTestCell) Name() string { return c.name }
func (c scopedTestCell) Scope() Scope { return c.scope }

type legacyTestCell struct{ name string }

func (c legacyTestCell) Name() string { return c.name }

func TestNewScopeValidationAndImmutability(t *testing.T) {
	if _, err := NewScope("", "instance", "cell", "cell-instance"); err == nil {
		t.Fatal("NewScope accepted an empty application ID")
	}
	if _, err := NewScope("app", "instance", "cell", "\x00"); err == nil {
		t.Fatal("NewScope accepted a NUL-containing cell instance ID")
	}

	scope, err := NewScope("evolution", "prod-a", "sessions", "worker-1")
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if got, want := scope.ApplicationID(), "evolution"; got != want {
		t.Fatalf("ApplicationID = %q, want %q", got, want)
	}
	if got, want := scope.CellInstanceID(), "worker-1"; got != want {
		t.Fatalf("CellInstanceID = %q, want %q", got, want)
	}
}

func TestLegacyScopePreservesBackwardCompatibleDefault(t *testing.T) {
	scope := LegacyScope("sessions")
	if got, want := scope.ApplicationID(), "legacy"; got != want {
		t.Fatalf("ApplicationID = %q, want %q", got, want)
	}
	if got, want := scope.ApplicationInstanceID(), "default"; got != want {
		t.Fatalf("ApplicationInstanceID = %q, want %q", got, want)
	}
	if got, want := scope.CellID(), "sessions"; got != want {
		t.Fatalf("CellID = %q, want %q", got, want)
	}
}

func TestScopeOfAndCellIDOfUseScopedIdentityWithoutBreakingLegacyCells(t *testing.T) {
	legacy := legacyTestCell{name: "sessions"}
	if got, want := ScopeOf(legacy), LegacyScope("sessions"); got != want {
		t.Fatalf("ScopeOf(legacy) = %#v, want %#v", got, want)
	}
	if got, want := CellIDOf(legacy), "sessions"; got != want {
		t.Fatalf("CellIDOf(legacy) = %q, want %q", got, want)
	}
	legacyScoped := scopedTestCell{name: "sessions", scope: LegacyScope("sessions")}
	if got, want := CellIDOf(legacyScoped), "sessions"; got != want {
		t.Fatalf("CellIDOf(legacy scoped) = %q, want %q", got, want)
	}

	scope, err := NewScope("evolution", "prod-a", "sessions", "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	scoped := scopedTestCell{name: "sessions", scope: scope}
	if got, want := ScopeOf(scoped), scope; got != want {
		t.Fatalf("ScopeOf(scoped) = %#v, want %#v", got, want)
	}
	if got, want := CellIDOf(scoped), scope.RoutingID(); got != want {
		t.Fatalf("CellIDOf(scoped) = %q, want %q", got, want)
	}
}

func TestResourceKeysDoNotCollideAcrossApplicationsOrInstances(t *testing.T) {
	keys := make(map[ResourceKey]struct{})
	for _, input := range [][4]string{
		{"evolution", "a", "sessions", "one"},
		{"evolution", "b", "sessions", "one"},
		{"sessions", "a", "sessions", "one"},
		{"evolution", "a", "sessions", "two"},
	} {
		scope, err := NewScope(input[0], input[1], input[2], input[3])
		if err != nil {
			t.Fatal(err)
		}
		key, err := scope.ResourceKey("storage", "primary")
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := keys[key]; exists {
			t.Fatalf("resource key collision for %s", key)
		}
		keys[key] = struct{}{}
	}
	if got, want := len(keys), 4; got != want {
		t.Fatalf("keys = %d, want %d", got, want)
	}
}

func TestResourceKeyStringIsInjectiveAcrossDelimiterContainingIDs(t *testing.T) {
	first, err := NewScope("a|1:b", "c", "d", "e")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewScope("a", "1:b|c", "d", "e")
	if err != nil {
		t.Fatal(err)
	}
	firstKey, err := first.ResourceKey("storage", "primary")
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := second.ResourceKey("storage", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if firstKey.String() == secondKey.String() {
		t.Fatalf("resource key strings collided: %s", firstKey)
	}
	if !strings.Contains(firstKey.String(), "5:a|1:b") {
		t.Fatalf("key representation omitted length prefix: %s", firstKey)
	}
}

func TestScopedFactorySeparatesMutableResourcesByScope(t *testing.T) {
	type handle struct{ sequence int }
	created := 0
	factory := NewScopedFactory(func(ResourceKey) (*handle, error) {
		created++
		return &handle{sequence: created}, nil
	})

	makeKey := func(applicationInstanceID, cellInstanceID string) ResourceKey {
		t.Helper()
		scope, err := NewScope("sessions", applicationInstanceID, "player-manager", cellInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		key, err := scope.ResourceKey("player-manager", "active")
		if err != nil {
			t.Fatal(err)
		}
		return key
	}

	firstKey := makeKey("app-a", "one")
	secondKey := makeKey("app-a", "two")
	thirdKey := makeKey("app-b", "one")
	first, createdFirst, err := factory.GetOrCreate(firstKey)
	if err != nil || !createdFirst {
		t.Fatalf("first GetOrCreate = (%v, %v, %v)", first, createdFirst, err)
	}
	again, createdAgain, err := factory.GetOrCreate(firstKey)
	if err != nil || createdAgain || again != first {
		t.Fatalf("same scope did not retain its handle: (%v, %v, %v)", again, createdAgain, err)
	}
	second, _, err := factory.GetOrCreate(secondKey)
	if err != nil || second == first {
		t.Fatalf("cell instances shared a mutable handle: (%v, %v)", second, err)
	}
	third, _, err := factory.GetOrCreate(thirdKey)
	if err != nil || third == first || third == second {
		t.Fatalf("application instances shared a mutable handle: (%v, %v)", third, err)
	}
	if got, want := factory.Count(), 3; got != want {
		t.Fatalf("Count = %d, want %d", got, want)
	}
}
