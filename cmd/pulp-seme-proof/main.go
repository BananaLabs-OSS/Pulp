// pulp-seme-proof is the deployment binary for Seme's first real Pulp target
// conformance proof. It adds one capability-shaped logging host import to the
// normal Pulp runtime without changing Pulp core.
package main

import (
	"context"
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/run"
	"github.com/tetratelabs/wazero"
)

func init() {
	ext.Register(ext.Capability{
		Name:     "observability.log",
		Provider: "seme.pulp.log-v1",
		Register: bindLog,
		Stub:     bindLogStub,
	})
}

func bindLog(builder wazero.HostModuleBuilder, cell ext.Cell) error {
	builder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, value uint32) uint32 {
			fmt.Printf("[observability.log] cell=%s quota.accepted=%t\n", cell.Name(), value != 0)
			return 0
		}).
		Export("log_bool")
	return nil
}

func bindLogStub(builder wazero.HostModuleBuilder, _ ext.Cell) error {
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, uint32) uint32 { return 99 }).
		Export("log_bool")
	return nil
}

func main() {
	run.Main()
}
