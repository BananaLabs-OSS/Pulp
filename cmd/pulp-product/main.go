// Command pulp-product is the compatibility alias for `pulp product`.
package main

import (
	"fmt"
	"os"

	"github.com/BananaLabs-OSS/Pulp/internal/productcli"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"plan"}
	}
	if err := productcli.Run(args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pulp-product:", err)
		os.Exit(1)
	}
}
