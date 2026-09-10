// Pulp — default CLI entry point. Loads a cell from a
// pulp.cell.toml manifest and runs it. All actual work lives in
// the Pulp/run package so that deployments can blank-import
// extensions and reuse the same Main function.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/BananaLabs-OSS/Pulp/internal/pulpcli"
	"github.com/BananaLabs-OSS/Pulp/run"
)

func main() {
	if pulpcli.IsCommand(os.Args[1:]) {
		if err := pulpcli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "pulp:", err)
			os.Exit(1)
		}
		return
	}
	run.Main()
}
