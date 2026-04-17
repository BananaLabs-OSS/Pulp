// Package demo is an example compile-time Pulp extension. A deployment
// that imports this package gets a "demo.greet" capability exposing a
// single host import — demo_greet(name_ptr, name_len) — that writes a
// greeting to plugin stdout. Useful as a template for real extensions
// (Pulp-ext-s3, Pulp-ext-stripe, etc.) and as an E2E smoke test for
// the ext.Register mechanism.
//
// Import path (blank):
//
//	import _ "github.com/BananaLabs-OSS/Pulp/ext/demo"
//
// Plugins declare capability "demo.greet" in their manifest.
package demo

import (
	"context"
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func init() {
	ext.Register(ext.Capability{
		Name:     "demo.greet",
		Register: bindActive,
		Stub:     bindStub,
	})
}

func bindActive(b wazero.HostModuleBuilder, p ext.Plugin) error {
	b.NewFunctionBuilder().
		WithFunc(func(_ context.Context, m api.Module, namePtr, nameLen uint32) uint32 {
			if nameLen == 0 {
				return 1
			}
			data, ok := m.Memory().Read(namePtr, nameLen)
			if !ok {
				return 2
			}
			fmt.Printf("[demo.greet] plugin=%s hello, %s!\n", p.Name(), string(data))
			return 0
		}).
		Export("demo_greet")
	return nil
}

func bindStub(b wazero.HostModuleBuilder, _ ext.Plugin) error {
	b.NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 99 }).
		Export("demo_greet")
	return nil
}
