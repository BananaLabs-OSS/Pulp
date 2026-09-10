package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type canonicalManifest struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	Version       string       `json:"version"`
	Dependencies  []Dependency `json:"dependencies"`
	Artifacts     []struct {
		Target             string          `json:"target"`
		Digest             string          `json:"digest"`
		Size               int64           `json:"size"`
		CellManifestDigest string          `json:"cell_manifest_digest,omitempty"`
		Source             *SourceArtifact `json:"source,omitempty"`
	} `json:"artifacts"`
	Provides     []string `json:"provides"`
	Consumes     []string `json:"consumes"`
	Capabilities []string `json:"capabilities"`
	FusionABI    string   `json:"fusion_abi,omitempty"`
}

func parseCanonical(b []byte) (Manifest, error) {
	var c canonicalManifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&c); e != nil {
		return Manifest{}, e
	}
	var trailing any
	if e := dec.Decode(&trailing); e != io.EOF {
		return Manifest{}, fmt.Errorf("trailing canonical manifest value")
	}
	if c.SchemaVersion != 1 {
		return Manifest{}, fmt.Errorf("invalid canonical manifest schema")
	}
	id, e := ParseModuleID(c.ID)
	if e != nil {
		return Manifest{}, e
	}
	v, e := ParseVersion(c.Version)
	if e != nil {
		return Manifest{}, e
	}
	m := Manifest{SchemaVersion: c.SchemaVersion, ID: id, Version: v, Dependencies: c.Dependencies, Provides: c.Provides, Consumes: c.Consumes, Capabilities: c.Capabilities, FusionABI: c.FusionABI}
	for _, a := range c.Artifacts {
		d, e := ParseDigest(a.Digest)
		if e != nil {
			return Manifest{}, e
		}
		m.Artifacts = append(m.Artifacts, Artifact{Target: a.Target, Digest: d, DigestText: d.String(), Size: a.Size, CellManifestDigest: a.CellManifestDigest, Source: a.Source})
	}
	canonical, e := CanonicalManifest(m)
	if e != nil {
		return Manifest{}, e
	}
	if !bytes.Equal(canonical, b) {
		return Manifest{}, fmt.Errorf("module manifest is not canonical")
	}
	return m, nil
}
