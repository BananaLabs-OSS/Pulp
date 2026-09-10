package registry

import "fmt"

// RefreshArtifacts returns canonical manifest bytes whose artifact digests
// and sizes exactly describe files supplied by target. The input Manifest is
// left unchanged.
func RefreshArtifacts(manifest Manifest, blobs map[string][]byte) ([]byte, map[Digest][]byte, error) {
	copyManifest := manifest
	copyManifest.Artifacts = append([]Artifact(nil), manifest.Artifacts...)
	content := make(map[Digest][]byte, len(copyManifest.Artifacts))
	for i := range copyManifest.Artifacts {
		artifact := &copyManifest.Artifacts[i]
		blob, ok := blobs[artifact.Target]
		if !ok {
			return nil, nil, fmt.Errorf("missing artifact bytes for target %q", artifact.Target)
		}
		artifact.Digest = Sum(blob)
		artifact.DigestText = artifact.Digest.String()
		artifact.Size = int64(len(blob))
		content[artifact.Digest] = append([]byte(nil), blob...)
	}
	if len(blobs) != len(copyManifest.Artifacts) {
		return nil, nil, fmt.Errorf("artifact bytes include an undeclared target")
	}
	wire, err := ManifestTOML(copyManifest)
	if err != nil {
		return nil, nil, err
	}
	return wire, content, nil
}
