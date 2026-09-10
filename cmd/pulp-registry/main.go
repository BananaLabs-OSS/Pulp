package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appmanifest "github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pulp-registry:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: pulp-registry <publish|publish-cell|refresh|refresh-app|resolve|serve|sync|verify> [flags]")
	}
	switch args[0] {
	case "publish":
		return publish(ctx, args[1:], stdout, stderr)
	case "publish-cell":
		return publishCell(ctx, args[1:], stdout, stderr)
	case "refresh":
		return refresh(args[1:], stdout, stderr)
	case "refresh-app":
		return refreshApp(args[1:], stdout, stderr)
	case "resolve":
		return resolve(ctx, args[1:], stdout, stderr)
	case "serve":
		return serve(ctx, args[1:], stdout, stderr)
	case "verify":
		return verify(ctx, args[1:], stdout, stderr)
	case "sync":
		return syncProject(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func publishCell(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, remotes := flags("publish-cell", stderr)
	cellPath := f.String("manifest", "", "pulp.cell.toml path (required)")
	moduleText := f.String("module", "", "registry module ID (required)")
	target := f.String("target", "wasm32-wasip1+pulp-v1", "artifact target")
	tokenFile := f.String("token-file", "", "publishing bearer-token file (required with -remote)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *cellPath == "" || *moduleText == "" {
		return errors.New("-manifest and -module are required")
	}
	moduleID, err := registry.ParseModuleID(*moduleText)
	if err != nil {
		return err
	}
	cell, err := appmanifest.Load(*cellPath)
	if err != nil {
		return err
	}
	version, err := registry.ParseVersion(cell.Version)
	if err != nil {
		return err
	}
	wasm, err := os.ReadFile(cell.WASMPath)
	if err != nil {
		return err
	}
	digest := registry.Sum(wasm)
	if cell.WASMSHA256 != "" && !strings.EqualFold(cell.WASMSHA256, strings.TrimPrefix(digest.String(), "sha256:")) {
		return fmt.Errorf("cell Wasm digest mismatch: manifest has %s, artifact has %s", cell.WASMSHA256, digest.String())
	}
	manifest := registry.Manifest{
		SchemaVersion: 1,
		ID:            moduleID,
		Version:       version,
		Provides:      cell.Provides,
		Consumes:      cell.Consumes,
		Capabilities:  cell.Capabilities,
		FusionABI:     cell.Execution.ABI,
		Artifacts: []registry.Artifact{{
			Target:     strings.TrimSpace(*target),
			Digest:     digest,
			DigestText: digest.String(),
			Size:       int64(len(wasm)),
		}},
	}
	wire, err := registry.ManifestTOML(manifest)
	if err != nil {
		return err
	}
	publisher, err := publicationTarget(*root, *remotes, *tokenFile)
	if err != nil {
		return err
	}
	if err := publisher.Publish(ctx, wire, map[registry.Digest][]byte{digest: wasm}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, registry.ReleaseID{ID: moduleID, Version: version})
	return nil
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	f.SetOutput(stderr)
	root := f.String("registry", "", "local registry directory (required)")
	listen := f.String("listen", "127.0.0.1:8080", "private HTTP listen address")
	tlsCert := f.String("tls-cert", "", "TLS certificate path")
	tlsKey := f.String("tls-key", "", "TLS private-key path")
	publishTokenFile := f.String("publish-token-file", "", "bearer-token file enabling authenticated publication")
	publishNamespace := f.String("publish-namespace", "bananalabs", "module namespace accepted by the publishing credential")
	if err := f.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*root) == "" {
		return errors.New("-registry is required for serve")
	}
	store, err := registry.OpenLocal(*root)
	if err != nil {
		return err
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return errors.New("-tls-cert and -tls-key must be supplied together")
	}
	var publishToken []byte
	if strings.TrimSpace(*publishTokenFile) != "" {
		publishToken, err = os.ReadFile(*publishTokenFile)
		if err != nil {
			return fmt.Errorf("read publish token: %w", err)
		}
		publishToken = []byte(strings.TrimSpace(string(publishToken)))
		if len(publishToken) < 32 {
			return errors.New("publish token must contain at least 32 bytes")
		}
	}
	handler := registryServerHandler(store, publishToken, *publishNamespace)
	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		case <-stopped:
		}
	}()
	fmt.Fprintf(stdout, "serving read-only registry %s from %s\n", *listen, *root)
	if *tlsCert != "" {
		err = server.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}

func registryServerHandler(store *registry.LocalStore, publishToken []byte, publishNamespace string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	if len(publishToken) != 0 {
		mux.HandleFunc("POST /v1/releases", authenticatedPublisher(store, publishToken, publishNamespace))
	}
	mux.Handle("/", registry.Handler(store))
	return mux
}

type remotePublishRequest struct {
	Manifest string            `json:"manifest"`
	Blobs    map[string][]byte `json:"blobs"`
}

func authenticatedPublisher(store *registry.LocalStore, token []byte, namespace string) http.HandlerFunc {
	want := sha256.Sum256(token)
	prefix := strings.Trim(strings.TrimSpace(namespace), "/") + "/"
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		got := sha256.Sum256([]byte(provided))
		if provided == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request remotePublishRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid publish request", http.StatusBadRequest)
			return
		}
		manifest, err := registry.ParseManifest([]byte(request.Manifest))
		if err != nil {
			http.Error(w, "invalid manifest", http.StatusBadRequest)
			return
		}
		if prefix == "/" || !strings.HasPrefix(string(manifest.ID), prefix) {
			http.Error(w, "module namespace forbidden", http.StatusForbidden)
			return
		}
		blobs := make(map[registry.Digest][]byte, len(request.Blobs))
		for rawDigest, data := range request.Blobs {
			digest, err := registry.ParseDigest(rawDigest)
			if err != nil || registry.Sum(data) != digest {
				http.Error(w, "invalid blob digest", http.StatusBadRequest)
				return
			}
			blobs[digest] = data
		}
		if err := store.Publish(r.Context(), []byte(request.Manifest), blobs); err != nil {
			http.Error(w, "publish failed: "+err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}

func refreshApp(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("refresh-app", flag.ContinueOnError)
	f.SetOutput(stderr)
	manifestPath := f.String("manifest", "pulp.app.toml", "pulp.app.toml path")
	if err := f.Parse(args); err != nil {
		return err
	}
	if err := appmanifest.RefreshAppDigests(*manifestPath); err != nil {
		return err
	}
	fmt.Fprintln(stdout, *manifestPath)
	return nil
}

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }

func flags(name string, stderr io.Writer) (*flag.FlagSet, *string, *values) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	root := f.String("registry", "", "local registry/cache directory (default: user cache)")
	remotes := &values{}
	f.Var(remotes, "remote", "hosted registry base URL (repeatable)")
	return f, root, remotes
}
func open(root string) (*registry.LocalStore, error) {
	if strings.TrimSpace(root) == "" {
		return registry.OpenDefaultLocal()
	}
	return registry.OpenLocal(root)
}

func cached(root string, remoteURLs []string) (registry.Store, error) {
	local, err := open(root)
	if err != nil {
		return nil, err
	}
	if len(remoteURLs) == 0 {
		return local, nil
	}
	store := &registry.CachedStore{Cache: local}
	for _, remoteURL := range remoteURLs {
		remote, err := registry.OpenHTTP(remoteURL, nil)
		if err != nil {
			return nil, err
		}
		store.Upstreams = append(store.Upstreams, remote)
	}
	return store, nil
}

func publicationTarget(root string, remoteURLs []string, tokenFile string) (registry.Publisher, error) {
	if len(remoteURLs) == 0 {
		return open(root)
	}
	if len(remoteURLs) != 1 {
		return nil, errors.New("publishing requires exactly one -remote")
	}
	if strings.TrimSpace(root) != "" {
		return nil, errors.New("-registry and publishing -remote are mutually exclusive")
	}
	if strings.TrimSpace(tokenFile) == "" {
		return nil, errors.New("-token-file is required for remote publishing")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read publishing token: %w", err)
	}
	return registry.OpenHTTPPublisher(remoteURLs[0], string(token), nil)
}

func publish(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, remotes := flags("publish", stderr)
	manifestPath := f.String("manifest", "", "pulp.module.toml path (required)")
	refreshDigests := f.Bool("refresh", false, "compute artifact digests/sizes and atomically refresh the manifest")
	tokenFile := f.String("token-file", "", "publishing bearer-token file (required with -remote)")
	var blobArgs values
	f.Var(&blobArgs, "blob", "artifact bytes as TARGET=PATH (repeatable)")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *manifestPath == "" {
		return errors.New("-manifest is required")
	}
	publisher, e := publicationTarget(*root, *remotes, *tokenFile)
	if e != nil {
		return e
	}
	raw, e := os.ReadFile(*manifestPath)
	if e != nil {
		return e
	}
	m, e := registry.ParseManifest(raw)
	if e != nil {
		return e
	}
	paths := map[string]string{}
	for _, entry := range blobArgs {
		target, path, ok := strings.Cut(entry, "=")
		if !ok || target == "" || path == "" {
			return fmt.Errorf("invalid -blob %q; want TARGET=PATH", entry)
		}
		if _, dup := paths[target]; dup {
			return fmt.Errorf("duplicate -blob target %q", target)
		}
		paths[target] = path
	}
	byTarget := map[string][]byte{}
	for _, a := range m.Artifacts {
		path, ok := paths[a.Target]
		if !ok {
			return fmt.Errorf("missing -blob for target %q", a.Target)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return fmt.Errorf("read blob %s: %w", a.Target, e)
		}
		byTarget[a.Target] = b
		delete(paths, a.Target)
	}
	if len(paths) > 0 {
		return errors.New("-blob target is not declared by manifest")
	}
	blobs := map[registry.Digest][]byte{}
	if *refreshDigests {
		raw, blobs, e = registry.RefreshArtifacts(m, byTarget)
		if e != nil {
			return e
		}
		m, e = registry.ParseManifest(raw)
		if e != nil {
			return e
		}
	} else {
		for _, a := range m.Artifacts {
			blobs[a.Digest] = byTarget[a.Target]
		}
	}
	if e = publisher.Publish(ctx, raw, blobs); e != nil {
		return e
	}
	if *refreshDigests {
		if e = registry.AtomicWrite(*manifestPath, raw, 0o644); e != nil {
			return fmt.Errorf("published, but refresh manifest: %w", e)
		}
	}
	fmt.Fprintln(stdout, registry.ReleaseID{ID: m.ID, Version: m.Version}.String())
	return nil
}

func refresh(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("refresh", flag.ContinueOnError)
	f.SetOutput(stderr)
	manifestPath := f.String("manifest", "", "pulp.module.toml path (required)")
	var blobArgs values
	f.Var(&blobArgs, "blob", "artifact bytes as TARGET=PATH (repeatable)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" {
		return errors.New("-manifest is required")
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	manifest, err := registry.ParseManifest(raw)
	if err != nil {
		return err
	}
	paths, err := parseBlobPaths(blobArgs)
	if err != nil {
		return err
	}
	byTarget := map[string][]byte{}
	for _, artifact := range manifest.Artifacts {
		path, ok := paths[artifact.Target]
		if !ok {
			return fmt.Errorf("missing -blob for target %q", artifact.Target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		byTarget[artifact.Target] = data
		delete(paths, artifact.Target)
	}
	if len(paths) != 0 {
		return errors.New("-blob target is not declared by manifest")
	}
	wire, _, err := registry.RefreshArtifacts(manifest, byTarget)
	if err != nil {
		return err
	}
	if err = registry.AtomicWrite(*manifestPath, wire, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, *manifestPath)
	return nil
}

func parseBlobPaths(entries []string) (map[string]string, error) {
	paths := map[string]string{}
	for _, entry := range entries {
		target, path, ok := strings.Cut(entry, "=")
		if !ok || target == "" || path == "" {
			return nil, fmt.Errorf("invalid -blob %q; want TARGET=PATH", entry)
		}
		if _, duplicate := paths[target]; duplicate {
			return nil, fmt.Errorf("duplicate -blob target %q", target)
		}
		paths[target] = path
	}
	return paths, nil
}
func resolve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, remotes := flags("resolve", stderr)
	target := f.String("target", "", "artifact target (required)")
	out := f.String("out", "", "lockfile output; default stdout")
	var roots values
	f.Var(&roots, "root", "root requirement as MODULE@CONSTRAINT (repeatable)")
	if e := f.Parse(args); e != nil {
		return e
	}
	s, e := cached(*root, *remotes)
	if e != nil {
		return e
	}
	if *target == "" {
		return errors.New("-target is required")
	}
	if len(roots) == 0 {
		return errors.New("at least one -root is required")
	}
	req := registry.Request{Target: *target}
	for _, entry := range roots {
		idText, constraint, ok := strings.Cut(entry, "@")
		if !ok {
			return fmt.Errorf("invalid -root %q; want MODULE@CONSTRAINT", entry)
		}
		id, e := registry.ParseModuleID(idText)
		if e != nil {
			return e
		}
		req.Roots = append(req.Roots, registry.Dependency{ID: id, Requirement: registry.Requirement(constraint)})
	}
	lock, e := registry.Resolve(ctx, s, req)
	if e != nil {
		return e
	}
	wire, e := lock.Marshal()
	if e != nil {
		return e
	}
	wire = append(wire, '\n')
	if *out == "" {
		_, e = stdout.Write(wire)
		return e
	}
	return atomicWrite(*out, wire)
}
func verify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, remotes := flags("verify", stderr)
	lockPath := f.String("lock", "", "lockfile path (required)")
	if e := f.Parse(args); e != nil {
		return e
	}
	s, e := cached(*root, *remotes)
	if e != nil {
		return e
	}
	if *lockPath == "" {
		return errors.New("-lock is required")
	}
	b, e := os.ReadFile(*lockPath)
	if e != nil {
		return e
	}
	lock, e := registry.ParseLock(b)
	if e != nil {
		return e
	}
	if e = registry.Verify(ctx, s, lock); e != nil {
		return e
	}
	fmt.Fprintln(stdout, "verified")
	return nil
}

func syncProject(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f, root, remotes := flags("sync", stderr)
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
	store, err := cached(*root, allRemotes)
	if err != nil {
		return err
	}
	lock, changed, err := registry.EnsureProjectLock(ctx, store, project, *lockPath)
	if err != nil {
		return err
	}
	if !changed {
		if err := registry.Verify(ctx, store, lock); err != nil {
			return err
		}
	}
	if changed {
		fmt.Fprintln(stdout, "updated", *lockPath)
	} else {
		fmt.Fprintln(stdout, "verified", *lockPath)
	}
	return nil
}
func atomicWrite(path string, data []byte) error {
	f, e := os.CreateTemp(filepathDir(path), ".pulp-lock-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func filepathDir(path string) string {
	i := strings.LastIndexAny(path, "/\\")
	if i < 0 {
		return "."
	}
	if i == 0 {
		return path[:1]
	}
	return path[:i]
}
