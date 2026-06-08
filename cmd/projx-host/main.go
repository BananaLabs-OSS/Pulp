// Command projx-host is a standalone Pulp host for the projx editor cell:
// run.Main + the ext-http transport. It serves the projx cell's HTTP API + the
// browser editor UI over a real socket.
//
// Build & run:
//
//	go build -o projx-host ./cmd/projx-host
//	./projx-host -manifest ../projx/cell/pulp.cell.toml -http-port 8080
//	# then open http://localhost:8080
//
// (cell.wasm must be built next to the manifest:
//
//	cd ../projx/cell && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o cell.wasm .
//
// This is a convenience launcher living in the Pulp repo so it reuses Pulp's
// existing ext replace graph; relocate to a deployment module when desired.
package main

import (
	"github.com/BananaLabs-OSS/Pulp/run"

	// Transport the projx cell declares (transport.http.inbound). Blank-import
	// registers the capability into ext.All() so run.Main can bind it.
	_ "github.com/BananaLabs-OSS/Pulp-ext-http"
)

func main() {
	run.Main()
}
