// pulp-http-test is a test-only deployment binary that blank-imports
// Pulp-ext-http so the HTTP integration test can exercise the full
// path through a real HTTP server. The default cmd/pulp binary links
// no extensions on purpose — this one does.
package main

import (
	_ "github.com/BananaLabs-OSS/Pulp-ext-http"

	"github.com/BananaLabs-OSS/Pulp/run"
)

func main() {
	run.Main()
}
