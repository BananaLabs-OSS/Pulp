// Package productcli implements the native Pulp product command family.
package productcli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/product"
)

var commands = map[string]bool{
	"plan": true, "build": true, "package": true, "install": true,
	"activate": true, "rollback": true, "state": true,
}

// Run executes `pulp product <command>`.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || !commands[args[0]] {
		return errors.New("usage: pulp product <plan|build|package|install|activate|rollback|state> [flags]")
	}
	command, args := args[0], args[1:]
	flags := flag.NewFlagSet("pulp product "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	descriptor := flags.String("descriptor", "pulp.product.json", "path to the product descriptor")
	output := flags.String("output", ".pulp/product", "assembly output directory")
	surface := flags.String("surface", "", "surface id to assemble")
	mode := flags.String("mode", "linked", "assembly mode: linked or frozen")
	assemblyRoot := flags.String("assembly", ".pulp/product", "frozen assembly directory")
	store := flags.String("store", ".pulp/releases", "product release store")
	digest := flags.String("digest", "", "installed release digest")
	goCommand := flags.String("go", "go", "Go command used to build a focused product host")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("pulp product accepts no positional arguments")
	}

	switch command {
	case "install":
		value, err := product.InstallRelease(*assemblyRoot, *store)
		if err != nil {
			return err
		}
		return emit(stdout, map[string]string{"digest": value})
	case "activate":
		value, err := product.ActivateRelease(*store, *digest)
		if err != nil {
			return err
		}
		return emit(stdout, value)
	case "rollback":
		value, err := product.RollbackRelease(*store)
		if err != nil {
			return err
		}
		return emit(stdout, value)
	case "state":
		value, err := product.LoadReleaseState(*store)
		if err != nil {
			return err
		}
		return emit(stdout, value)
	}

	plan, err := product.Resolve(*descriptor)
	if err != nil {
		return err
	}
	if command == "plan" {
		return emit(stdout, plan)
	}
	selected := *surface
	if selected == "" {
		selected = plan.Entrypoint.Surface
	}
	if command == "build" {
		var assembly product.Assembly
		switch *mode {
		case "linked":
			assembly, err = product.Assemble(*descriptor, *output, selected)
		case "frozen":
			assembly, err = product.AssembleFrozen(*descriptor, *output, selected)
		default:
			return fmt.Errorf("unsupported assembly mode %q", *mode)
		}
		if err != nil {
			return err
		}
		return emit(stdout, assembly)
	}

	outputRoot, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outputRoot), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(outputRoot), ".pulp-product-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	assembly, err := product.AssembleFrozen(*descriptor, stage, selected)
	if err != nil {
		return err
	}
	bootstrap, err := embeddedBootstrapSource(assembly.Root, plan.ID, plan.Version)
	if err != nil {
		return fmt.Errorf("build embedded product: %w", err)
	}
	bootstrapPath := filepath.Join(filepath.Dir(plan.HostModule), "zz_pulp_product_embedded.go")
	bootstrapFile, err := os.OpenFile(bootstrapPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("stage embedded product bootstrap: %w", err)
	}
	if _, err = bootstrapFile.Write(bootstrap); err != nil {
		bootstrapFile.Close()
		os.Remove(bootstrapPath)
		return err
	}
	if err = bootstrapFile.Close(); err != nil {
		os.Remove(bootstrapPath)
		return err
	}
	defer os.Remove(bootstrapPath)
	name := strings.ReplaceAll(plan.ID, ".", "-") + "-host"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	host := filepath.Join(assembly.Root, "bin", name)
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		return err
	}
	build := exec.Command(*goCommand, "build", "-trimpath", "-buildvcs=false", "-o", host, ".")
	build.Dir = filepath.Dir(plan.HostModule)
	build.Env = append(os.Environ(), "GOWORK=off")
	build.Stdout, build.Stderr = stderr, stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build focused product host: %w", err)
	}
	if err := activatePackage(stage, outputRoot); err != nil {
		return err
	}
	assembly.Root = outputRoot
	assembly.HostManifest = filepath.Join(outputRoot, "pulp.host.toml")
	assembly.PlanManifest = filepath.Join(outputRoot, "pulp.product.plan.json")
	assembly.LaunchManifest = filepath.Join(outputRoot, "pulp.product.launch.json")
	host = filepath.Join(outputRoot, "bin", name)
	return emit(stdout, map[string]any{"assembly": assembly, "host_executable": host})
}

func activatePackage(stage, output string) error {
	var backup string
	if _, err := os.Stat(output); err == nil {
		placeholder, err := os.MkdirTemp(filepath.Dir(output), ".pulp-product-backup-")
		if err != nil {
			return err
		}
		if err := os.Remove(placeholder); err != nil {
			return err
		}
		backup = placeholder
		if err := os.Rename(output, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, output); err != nil {
		if backup != "" {
			_ = os.Rename(backup, output)
		}
		return fmt.Errorf("activate packaged product: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove prior packaged product: %w", err)
		}
	}
	return nil
}

func emit(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
