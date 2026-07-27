package run

import (
	"context"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

func TestHostedEffectTestHostRejectsImplicitProductionInputs(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		options HostedEffectTestHostOptions
		want    string
	}{
		{name: "context", options: HostedEffectTestHostOptions{StorageRoot: t.TempDir(), Capabilities: map[string]ext.Capability{}}, want: "context is required"},
		{name: "storage", ctx: context.Background(), options: HostedEffectTestHostOptions{Capabilities: map[string]ext.Capability{}}, want: "storage root is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := StartHostedEffectTestHost(test.ctx, "ignored.toml", test.options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("StartHostedEffectTestHost error = %v, want %q", err, test.want)
			}
		})
	}
}
