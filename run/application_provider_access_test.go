package run

import (
	"context"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestApplicationProviderAccessRejectsForeignAndUndeclaredProviders(t *testing.T) {
	access := &applicationProviderAccess{identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}, active: true, runtimes: map[string]*cellRuntime{
		"commerce": {spec: &manifest.CellSpec{Name: "commerce", Provides: []string{"order.apply"}}},
	}}
	if _, err := access.CallProvider(context.Background(), "fleet", "order.apply", nil); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("foreign cell error = %v", err)
	}
	if _, err := access.CallProvider(context.Background(), "commerce", "fleet.apply", nil); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("uninitialized local cell error = %v", err)
	}
}

func TestApplicationProviderAccessRejectsCallsAfterRevocation(t *testing.T) {
	access := &applicationProviderAccess{identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}, active: true, runtimes: map[string]*cellRuntime{}}
	access.revoke()
	if _, err := access.CallProvider(context.Background(), "commerce", "order.apply", nil); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked access error = %v", err)
	}
}
