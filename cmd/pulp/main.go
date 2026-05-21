// Pulp — default CLI entry point. Loads a cell from a
// pulp.cell.toml manifest and runs it. All actual work lives in
// the Pulp/run package so that deployments can blank-import
// extensions and reuse the same Main function.
package main

import "github.com/BananaLabs-OSS/Pulp/run"

func main() {
	run.Main()
}
