package fusion

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

// RegistrySourceCatalog resolves source descriptors exclusively from verified
// registry manifests and content blobs.
type RegistrySourceCatalog struct{ Store registry.Store }

func (c RegistrySourceCatalog) FusionSource(ctx context.Context, release registry.ReleaseID) (Source, error) {
	if c.Store == nil {
		return Source{}, fmt.Errorf("registry source store is required")
	}
	manifest, err := c.Store.Manifest(ctx, release)
	if err != nil {
		return Source{}, err
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.Source == nil {
			continue
		}
		if artifact.Source.Language != "go" {
			continue
		}
		reader, err := c.Store.Blob(ctx, artifact.Digest)
		if err != nil {
			return Source{}, err
		}
		blob, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return Source{}, readErr
		}
		if closeErr != nil {
			return Source{}, closeErr
		}
		if registry.Sum(blob) != artifact.Digest || int64(len(blob)) != artifact.Size {
			return Source{}, fmt.Errorf("source artifact verification failed for %s", release)
		}
		// Registry versions omit Go's required leading v. Preserve the semantic
		// version while producing a version valid in the generated go.mod.
		return Source{ModulePath: artifact.Source.ModulePath, ImportPath: artifact.Source.ImportPath, Version: "v" + release.Version.String(), SourceSHA256: strings.TrimPrefix(artifact.Digest.String(), "sha256:"), Entrypoint: artifact.Source.Entrypoint}, nil
	}
	return Source{}, fmt.Errorf("%s has no Go source artifact", release)
}
