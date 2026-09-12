// Command pulp-product validates and resolves a standalone product assembled
// from a Pulp application, a focused host, and one or more presentation shells.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/BananaLabs-OSS/Pulp/product"
)

func main() {
	recipe := flag.String("descriptor", "pulp.product.json", "path to the product descriptor")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "pulp-product accepts no positional arguments")
		os.Exit(2)
	}
	plan, err := product.Resolve(*recipe)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pulp-product:", err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(plan); err != nil {
		fmt.Fprintln(os.Stderr, "pulp-product:", err)
		os.Exit(1)
	}
}
