// Command pulp-cache-warm compiles every WASM module below one or more paths
// into Pulp's persistent wazero cache without instantiating or initializing a
// cell. It is intended for immutable image builds: the resulting cache can be
// copied into the runtime image and used on the first container start.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/tetratelabs/wazero"
)

func main() {
	cacheDir := flag.String("cache", "", "destination directory for the persistent wazero cache")
	flag.Parse()
	if *cacheDir == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: pulp-cache-warm -cache DIR PATH [PATH...]")
		os.Exit(2)
	}
	if err := warm(context.Background(), *cacheDir, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "warm pulp cache: %v\n", err)
		os.Exit(1)
	}
}

func warm(ctx context.Context, cacheDir string, roots []string) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	if err := os.Setenv("PULP_WAZERO_CACHE", cacheDir); err != nil {
		return err
	}
	paths, err := wasmPaths(roots)
	if err != nil {
		return err
	}
	runtime := host.NewRuntimeWithPersistentCompilationCache(ctx, wazero.NewRuntimeConfig())
	defer runtime.Close(ctx)
	for _, path := range paths {
		wasm, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		compiled, compileErr := runtime.CompileModule(ctx, wasm)
		if compileErr != nil {
			return fmt.Errorf("compile %s: %w", path, compileErr)
		}
		if closeErr := compiled.Close(ctx); closeErr != nil {
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
	}
	fmt.Printf("warmed %d WASM modules in %s\n", len(paths), cacheDir)
	return nil
}

func wasmPaths(roots []string) ([]string, error) {
	unique := make(map[string]struct{})
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if strings.EqualFold(filepath.Ext(root), ".wasm") {
				unique[root] = struct{}{}
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".wasm") {
				unique[path] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(unique) == 0 {
		return nil, errors.New("no WASM modules found")
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
