// Package pulpcli implements package-oriented commands for the unified Pulp
// executable. Runtime flag handling remains in package run.
package pulpcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/fusion"
	appmanifest "github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
	"github.com/BananaLabs-OSS/Pulp/run"
)

var commands = map[string]bool{
	"sync": true, "update": true, "inspect": true, "publish": true,
	"refresh": true, "refresh-app": true, "digest-gate": true, "verify": true, "rollback": true, "recovery": true,
	"import-go": true, "inspect-app": true,
}

// IsCommand reports whether args select a unified package command.
func IsCommand(args []string) bool { return len(args) > 0 && commands[args[0]] }

// Run executes a unified package command.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: pulp <sync|update|inspect|publish|refresh|refresh-app|verify|rollback> [flags]")
	}
	switch args[0] {
	case "sync":
		return syncProject(ctx, args[1:], stdout, stderr, false)
	case "update":
		return syncProject(ctx, args[1:], stdout, stderr, true)
	case "inspect":
		return inspect(args[1:], stdout, stderr)
	case "inspect-app":
		return inspectApp(args[1:], stdout, stderr)
	case "publish":
		return publish(ctx, args[1:], stdout, stderr)
	case "refresh":
		return refresh(args[1:], stdout, stderr)
	case "refresh-app":
		return refreshApp(args[1:], stdout, stderr)
	case "digest-gate":
		return digestGate(args[1:], stdout, stderr)
	case "import-go":
		return importGo(ctx, args[1:], stdout, stderr)
	case "verify":
		return verify(ctx, args[1:], stdout, stderr)
	case "rollback":
		return recovery(args[1:], stdout, stderr, true)
	case "recovery":
		return recovery(args[1:], stdout, stderr, false)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// inspectApp exposes the validated application DAG without starting any cell.
// Its JSON is deliberately derived from the same immutable Plan the runtime
// uses, so tooling does not need a second dependency-graph implementation.
func inspectApp(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("inspect-app", flag.ContinueOnError)
	f.SetOutput(stderr)
	path := f.String("manifest", "pulp.app.toml", "application manifest")
	jsonOutput := f.Bool("json", false, "emit machine-readable JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	app, err := appmanifest.LoadApp(*path)
	if err != nil {
		return err
	}
	type plannedCell struct {
		ID           string   `json:"id"`
		Ordinal      int      `json:"ordinal"`
		Level        int      `json:"level"`
		Dependencies []string `json:"dependencies"`
		Dependents   []string `json:"dependents"`
		Provides     []string `json:"provides,omitempty"`
		Consumes     []string `json:"consumes,omitempty"`
	}
	type applicationPlan struct {
		Schema         string        `json:"schema"`
		Application    string        `json:"application"`
		Version        string        `json:"version"`
		ReadySemantics string        `json:"ready_semantics"`
		Levels         [][]string    `json:"levels"`
		StartOrder     []string      `json:"start_order"`
		StopOrder      []string      `json:"stop_order"`
		Cells          []plannedCell `json:"cells"`
	}
	plan := applicationPlan{
		Schema: "pulp.application-dependency-plan/v1", Application: app.Name, Version: app.Version,
		ReadySemantics: "a cell may initialize only after all direct dependencies are ready; independent ready cells may initialize in parallel",
		Levels:         app.Cells.Plan.Levels(), StartOrder: app.Cells.Plan.StartOrder(), StopOrder: app.Cells.Plan.StopOrder(),
	}
	for _, spec := range app.Cells.Cells {
		node, _ := app.Cells.Plan.Node(spec.Name)
		plan.Cells = append(plan.Cells, plannedCell{
			ID: spec.Name, Ordinal: node.Ordinal, Level: node.Level,
			Dependencies: node.Dependencies(), Dependents: node.Dependents(),
			Provides: append([]string(nil), spec.Provides...), Consumes: append([]string(nil), spec.Consumes...),
		})
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(plan)
	}
	fmt.Fprintf(stdout, "%s@%s\n", plan.Application, plan.Version)
	for level, ids := range plan.Levels {
		fmt.Fprintf(stdout, "level %d: %s\n", level+1, strings.Join(ids, ", "))
	}
	fmt.Fprintln(stdout, "start:", strings.Join(plan.StartOrder, " -> "))
	fmt.Fprintln(stdout, "stop:", strings.Join(plan.StopOrder, " -> "))
	return nil
}

func digestGate(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("digest-gate", flag.ContinueOnError)
	f.SetOutput(stderr)
	host := f.String("host", "pulp.host.toml", "production host manifest")
	generate := f.Bool("generate", false, "deliberately refresh every reachable Lua/Wasm digest before checking")
	if err := f.Parse(args); err != nil {
		return err
	}
	var err error
	if *generate {
		err = appmanifest.RefreshHostDigests(*host)
	} else {
		err = appmanifest.CheckHostDigests(*host)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, *host)
	return nil
}

func importGo(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("import-go", flag.ContinueOnError)
	f.SetOutput(stderr)
	goModPath := f.String("go-mod", "go.mod", "Go module file")
	goSumPath := f.String("go-sum", "go.sum", "Go checksum file")
	output := f.String("out", ".pulp/go-sources", "source-lock output directory")
	proxy := f.String("proxy", "", "GOPROXY override")
	if err := f.Parse(args); err != nil {
		return err
	}
	goMod, err := os.ReadFile(*goModPath)
	if err != nil {
		return err
	}
	goSum, err := os.ReadFile(*goSumPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lock, err := (fusion.GoImporter{Proxy: *proxy}).Import(ctx, goMod, goSum)
	if err != nil {
		return err
	}
	if err := lock.Save(*output); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s (%d modules)\n", *output, len(lock.Dependencies))
	return nil
}

func recovery(args []string, stdout, stderr io.Writer, rollback bool) error {
	name := "recovery"
	if rollback {
		name = "rollback"
	}
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	historyPath := f.String("history", "", "deployment history journal (required)")
	jsonOutput := f.Bool("json", false, "emit machine-readable JSON")
	apply := f.Bool("apply", false, "request live activation of the rollback target")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *historyPath == "" {
		return errors.New("-history is required")
	}
	if _, err := os.Stat(*historyPath); err != nil {
		return fmt.Errorf("open deployment history: %w", err)
	}
	history, err := run.OpenDeploymentHistory(*historyPath)
	if err != nil {
		return err
	}
	defer history.Close()
	state := history.Recovery()
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(state); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "last committed: %s\n", displayRevision(state.LastCommittedGraphRevision))
		fmt.Fprintf(stdout, "rollback target: %s\n", displayRevision(state.RollbackGraphRevision))
		if len(state.IncompleteTransactions) == 0 {
			fmt.Fprintln(stdout, "incomplete transactions: none")
		} else {
			for _, id := range state.IncompleteTransactions {
				fmt.Fprintln(stdout, "incomplete transaction:", id)
			}
		}
	}
	if *apply {
		return errors.New("live rollback transport is not configured; inspected history only and did not alter the runtime")
	}
	if rollback && state.RollbackGraphRevision == "" {
		return errors.New("deployment history has no prior graph to roll back to")
	}
	return nil
}

func displayRevision(revision string) string {
	if revision == "" {
		return "none"
	}
	return revision
}

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }

func storeFlags(name string, stderr io.Writer) (*flag.FlagSet, *string, *values) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	root := f.String("registry", "", "local registry/cache directory (default: user cache)")
	remotes := &values{}
	f.Var(remotes, "remote", "hosted registry base URL (repeatable)")
	return f, root, remotes
}

func openLocal(root string) (*registry.LocalStore, error) {
	if strings.TrimSpace(root) == "" {
		return registry.OpenDefaultLocal()
	}
	return registry.OpenLocal(root)
}

func openStore(root string, remotes []string) (registry.Store, error) {
	local, err := openLocal(root)
	if err != nil {
		return nil, err
	}
	if len(remotes) == 0 {
		return local, nil
	}
	cached := &registry.CachedStore{Cache: local}
	for _, raw := range remotes {
		remote, err := registry.OpenHTTP(raw, nil)
		if err != nil {
			return nil, err
		}
		cached.Upstreams = append(cached.Upstreams, remote)
	}
	return cached, nil
}

func syncProject(ctx context.Context, args []string, stdout, stderr io.Writer, force bool) error {
	f, root, remotes := storeFlags(map[bool]string{false: "sync", true: "update"}[force], stderr)
	projectPath := f.String("project", registry.DefaultProjectFile, "project module declaration")
	lockPath := f.String("lock", registry.DefaultLockFile, "resolved lockfile")
	if err := f.Parse(args); err != nil {
		return err
	}
	project, err := registry.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	allRemotes := append(append([]string(nil), project.Remotes...), (*remotes)...)
	store, err := openStore(*root, allRemotes)
	if err != nil {
		return err
	}
	if !force {
		_, changed, err := registry.EnsureProjectLock(ctx, store, project, *lockPath)
		if err != nil {
			return err
		}
		if changed {
			fmt.Fprintln(stdout, "updated", *lockPath)
		} else {
			fmt.Fprintln(stdout, "verified", *lockPath)
		}
		return nil
	}
	lock, err := registry.ResolveProject(ctx, store, project)
	if err != nil {
		return err
	}
	wire, err := lock.Marshal()
	if err != nil {
		return err
	}
	if err := registry.AtomicWrite(*lockPath, append(wire, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "updated", *lockPath)
	return nil
}

func inspect(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("inspect", flag.ContinueOnError)
	f.SetOutput(stderr)
	projectPath := f.String("project", registry.DefaultProjectFile, "project module declaration")
	lockPath := f.String("lock", registry.DefaultLockFile, "resolved lockfile")
	jsonOutput := f.Bool("json", false, "emit machine-readable JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	project, err := registry.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	wire, err := os.ReadFile(*lockPath)
	if err != nil {
		return err
	}
	lock, err := registry.ParseLock(wire)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(struct {
			Project *registry.Project `json:"project"`
			Lock    *registry.Lock    `json:"lock"`
		}{project, lock})
	}
	fmt.Fprintf(stdout, "target: %s\nroots: %d\nmodules: %d\n", lock.Target, len(project.Modules), len(lock.Modules))
	for _, m := range lock.Modules {
		fmt.Fprintf(stdout, "%s@%s %s\n", m.ID, m.Version, m.ArtifactDigest)
	}
	return nil
}

func refreshApp(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("refresh-app", flag.ContinueOnError)
	f.SetOutput(stderr)
	path := f.String("manifest", "pulp.app.toml", "application manifest")
	if err := f.Parse(args); err != nil {
		return err
	}
	if err := appmanifest.RefreshAppDigests(*path); err != nil {
		return err
	}
	fmt.Fprintln(stdout, *path)
	return nil
}

func blobPaths(entries []string) (map[string]string, error) {
	result := map[string]string{}
	for _, entry := range entries {
		target, path, ok := strings.Cut(entry, "=")
		if !ok || target == "" || path == "" {
			return nil, fmt.Errorf("invalid -blob %q; want TARGET=PATH", entry)
		}
		if _, exists := result[target]; exists {
			return nil, fmt.Errorf("duplicate -blob target %q", target)
		}
		result[target] = path
	}
	return result, nil
}

func loadBlobs(manifest *registry.Manifest, entries []string) (map[string][]byte, error) {
	paths, err := blobPaths(entries)
	if err != nil {
		return nil, err
	}
	result := map[string][]byte{}
	for _, artifact := range manifest.Artifacts {
		path, ok := paths[artifact.Target]
		if !ok {
			return nil, fmt.Errorf("missing -blob for target %q", artifact.Target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		result[artifact.Target] = data
		delete(paths, artifact.Target)
	}
	if len(paths) != 0 {
		keys := make([]string, 0, len(paths))
		for key := range paths {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("-blob target %q is not declared by manifest", keys[0])
	}
	return result, nil
}

func refresh(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("refresh", flag.ContinueOnError)
	f.SetOutput(stderr)
	path := f.String("manifest", "", "module manifest (required)")
	var blobs values
	f.Var(&blobs, "blob", "artifact as TARGET=PATH (repeatable)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("-manifest is required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	manifest, err := registry.ParseManifest(raw)
	if err != nil {
		return err
	}
	byTarget, err := loadBlobs(&manifest, blobs)
	if err != nil {
		return err
	}
	raw, _, err = registry.RefreshArtifacts(manifest, byTarget)
	if err != nil {
		return err
	}
	if err := registry.AtomicWrite(*path, raw, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, *path)
	return nil
}

func publish(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, _ := storeFlags("publish", stderr)
	path := f.String("manifest", "", "module manifest (required)")
	refreshFirst := f.Bool("refresh", false, "refresh artifact digests before publish")
	var blobs values
	f.Var(&blobs, "blob", "artifact as TARGET=PATH (repeatable)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("-manifest is required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	manifest, err := registry.ParseManifest(raw)
	if err != nil {
		return err
	}
	byTarget, err := loadBlobs(&manifest, blobs)
	if err != nil {
		return err
	}
	byDigest := map[registry.Digest][]byte{}
	if *refreshFirst {
		raw, byDigest, err = registry.RefreshArtifacts(manifest, byTarget)
		if err != nil {
			return err
		}
		manifest, err = registry.ParseManifest(raw)
		if err != nil {
			return err
		}
	} else {
		for _, artifact := range manifest.Artifacts {
			byDigest[artifact.Digest] = byTarget[artifact.Target]
		}
	}
	store, err := openLocal(*root)
	if err != nil {
		return err
	}
	if err := store.Publish(ctx, raw, byDigest); err != nil {
		return err
	}
	if *refreshFirst {
		if err := registry.AtomicWrite(*path, raw, 0o644); err != nil {
			return fmt.Errorf("published, but refresh manifest: %w", err)
		}
	}
	fmt.Fprintln(stdout, registry.ReleaseID{ID: manifest.ID, Version: manifest.Version})
	return nil
}

func verify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, remotes := storeFlags("verify", stderr)
	path := f.String("lock", registry.DefaultLockFile, "lockfile")
	if err := f.Parse(args); err != nil {
		return err
	}
	store, err := openStore(*root, *remotes)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	lock, err := registry.ParseLock(raw)
	if err != nil {
		return err
	}
	if err := registry.Verify(ctx, store, lock); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "verified", *path)
	return nil
}
