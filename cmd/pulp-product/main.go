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
	if len(args) != 0 && (args[0] == "plan" || args[0] == "build" || args[0] == "install" || args[0] == "activate" || args[0] == "rollback" || args[0] == "state") {
		command, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("pulp-product "+command, flag.ContinueOnError)
	descriptor := flags.String("descriptor", "pulp.product.json", "path to the product descriptor")
	output := flags.String("output", ".pulp/product", "assembly output directory")
	surface := flags.String("surface", "", "surface id to assemble")
	mode := flags.String("mode", "linked", "assembly mode: linked or frozen")
	assemblyRoot := flags.String("assembly", ".pulp/product", "frozen assembly directory")
	store := flags.String("store", ".pulp/releases", "product release store")
	digest := flags.String("digest", "", "installed release digest")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(os.Stderr, "pulp-product accepts no positional arguments")
		}
		os.Exit(2)
	}
	if command == "install" {
		value, err := product.InstallRelease(*assemblyRoot, *store)
		if err != nil {
			fail(err)
		}
		emit(map[string]string{"digest": value})
		return
	}
	if command == "activate" {
		value, err := product.ActivateRelease(*store, *digest)
		if err != nil {
			fail(err)
		}
		emit(value)
		return
	}
	if command == "rollback" {
		value, err := product.RollbackRelease(*store)
		if err != nil {
			fail(err)
		}
		emit(value)
		return
	}
	if command == "state" {
		value, err := product.LoadReleaseState(*store)
		if err != nil {
			fail(err)
		}
		emit(value)
		return
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
	emit(value)
}

func emit(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "pulp-product:", err)
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pulp-product:", err)
	os.Exit(1)
}
