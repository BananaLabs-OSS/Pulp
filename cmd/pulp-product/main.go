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
	command := "plan"
	args := os.Args[1:]
	if len(args) != 0 && (args[0] == "plan" || args[0] == "build") {
		command, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("pulp-product "+command, flag.ContinueOnError)
	descriptor := flags.String("descriptor", "pulp.product.json", "path to the product descriptor")
	output := flags.String("output", ".pulp/product", "assembly output directory")
	surface := flags.String("surface", "", "surface id to assemble")
	mode := flags.String("mode", "linked", "assembly mode: linked or frozen")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(os.Stderr, "pulp-product accepts no positional arguments")
		}
		os.Exit(2)
	}
	plan, err := product.Resolve(*descriptor)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pulp-product:", err)
		os.Exit(1)
	}
	value := any(plan)
	if command == "build" {
		selected := *surface
		if selected == "" {
			selected = plan.Entrypoint.Surface
		}
		var assembly product.Assembly
		var buildErr error
		switch *mode {
		case "linked":
			assembly, buildErr = product.Assemble(*descriptor, *output, selected)
		case "frozen":
			assembly, buildErr = product.AssembleFrozen(*descriptor, *output, selected)
		default:
			buildErr = fmt.Errorf("unsupported assembly mode %q", *mode)
		}
		if buildErr != nil {
			fmt.Fprintln(os.Stderr, "pulp-product:", buildErr)
			os.Exit(1)
		}
		value = assembly
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "pulp-product:", err)
		os.Exit(1)
	}
}
