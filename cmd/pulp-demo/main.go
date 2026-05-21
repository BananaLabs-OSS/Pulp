// pulp-demo is an example of a "deployment binary" — a custom Pulp
// build that statically links extensions. Real deployments will have
// their own repos with one main.go like this that blank-imports the
// extensions they need, plus a go.mod pinning versions.
//
// Run:
//
//	go build -o pulp-demo ./cmd/pulp-demo
//	./pulp-demo -manifest path/to/cell.toml
//
// This binary has everything the default cmd/pulp binary does PLUS
// the demo.greet capability provided by Pulp/ext/demo.
//
// For production deployments use the same pattern with
// Pulp-ext-s3, Pulp-ext-stripe, etc.
package main

import (
	_ "github.com/BananaLabs-OSS/Pulp/ext/demo"

	"github.com/BananaLabs-OSS/Pulp/run"
)

func main() {
	run.Main()
}
