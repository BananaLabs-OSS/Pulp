package ext

import "testing"

func TestSetupEnvEffectiveScopeUsesExplicitScopeAndLegacyFallback(t *testing.T) {
	explicit, err := NewScope("sessions", "primary", "host", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got := (SetupEnv{Scope: explicit, CellName: "ignored"}).EffectiveScope(); got != explicit {
		t.Fatalf("explicit EffectiveScope = %#v, want %#v", got, explicit)
	}
	if got, want := (SetupEnv{CellName: "sessions"}).EffectiveScope(), LegacyScope("sessions"); got != want {
		t.Fatalf("legacy EffectiveScope = %#v, want %#v", got, want)
	}
}
