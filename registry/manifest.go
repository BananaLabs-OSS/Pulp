package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// ManifestTOML serializes a validated manifest into deterministic publishable
// TOML. It is used when an upstream release is copied into a local cache.
func ManifestTOML(m Manifest) ([]byte, error) {
	canonical, err := CanonicalManifest(m)
	if err != nil {
		return nil, err
	}
	if _, err = parseCanonical(canonical); err != nil {
		return nil, err
	}
	quoted := func(values []string) string {
		values = normalized(values)
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = strconv.Quote(value)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "schema_version = 1\nprovides = %s\nconsumes = %s\ncapabilities = %s\n\n[module]\nid = %s\nversion = %s\n", quoted(m.Provides), quoted(m.Consumes), quoted(m.Capabilities), strconv.Quote(string(m.ID)), strconv.Quote(m.Version.String()))
	if m.FusionABI != "" {
		fmt.Fprintf(&out, "\n[execution]\nabi = %s\n", strconv.Quote(m.FusionABI))
	}
	for _, dependency := range m.Dependencies {
		fmt.Fprintf(&out, "\n[[dependencies]]\nid = %s\nrequirement = %s\n", strconv.Quote(string(dependency.ID)), strconv.Quote(string(dependency.Requirement)))
	}
	for _, artifact := range m.Artifacts {
		fmt.Fprintf(&out, "\n[[artifacts]]\ntarget = %s\ndigest = %s\nsize = %d\n", strconv.Quote(artifact.Target), strconv.Quote(artifact.Digest.String()), artifact.Size)
		if artifact.CellManifestDigest != "" {
			fmt.Fprintf(&out, "cell_manifest_digest = %s\n", strconv.Quote(artifact.CellManifestDigest))
		}
		if artifact.Source != nil {
			fmt.Fprintf(&out, "\n[artifacts.source]\nlanguage = %s\nmodule_path = %s\nimport_path = %s\nentrypoint = %s\n", strconv.Quote(artifact.Source.Language), strconv.Quote(artifact.Source.ModulePath), strconv.Quote(artifact.Source.ImportPath), strconv.Quote(artifact.Source.Entrypoint))
			if artifact.Source.Toolchain != "" {
				fmt.Fprintf(&out, "toolchain = %s\n", strconv.Quote(artifact.Source.Toolchain))
			}
		}
	}
	return []byte(out.String()), nil
}

type rawManifest struct {
	SchemaVersion int                                `toml:"schema_version"`
	Module        struct{ ID, Version string }       `toml:"module"`
	Dependencies  []struct{ ID, Requirement string } `toml:"dependencies"`
	Artifacts     []rawArtifact                      `toml:"artifacts"`
	Provides      []string                           `toml:"provides"`
	Consumes      []string                           `toml:"consumes"`
	Capabilities  []string                           `toml:"capabilities"`
	Execution     struct{ ABI string }               `toml:"execution"`
}
type rawArtifact struct {
	Target             string          `toml:"target"`
	Digest             string          `toml:"digest"`
	CellManifestDigest string          `toml:"cell_manifest_digest"`
	Size               int64           `toml:"size"`
	Source             *SourceArtifact `toml:"source"`
}

func ParseManifest(data []byte) (Manifest, error) {
	var raw rawManifest
	meta, err := toml.Decode(string(data), &raw)
	if err != nil {
		return Manifest{}, fmt.Errorf("parse module manifest: %w", err)
	}
	if u := meta.Undecoded(); len(u) > 0 {
		return Manifest{}, fmt.Errorf("unknown module manifest field %s", u[0])
	}
	if raw.SchemaVersion != 1 {
		return Manifest{}, fmt.Errorf("schema_version must be 1")
	}
	id, err := ParseModuleID(raw.Module.ID)
	if err != nil {
		return Manifest{}, err
	}
	v, err := ParseVersion(raw.Module.Version)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{SchemaVersion: 1, ID: id, Version: v, Provides: normalized(raw.Provides), Consumes: normalized(raw.Consumes), Capabilities: normalized(raw.Capabilities), FusionABI: strings.TrimSpace(raw.Execution.ABI)}
	for _, d := range raw.Dependencies {
		did, e := ParseModuleID(d.ID)
		if e != nil {
			return Manifest{}, e
		}
		req := Requirement(strings.TrimSpace(d.Requirement))
		if !validRequirement(req) {
			return Manifest{}, fmt.Errorf("invalid requirement %q", req)
		}
		m.Dependencies = append(m.Dependencies, Dependency{did, req})
	}
	for _, a := range raw.Artifacts {
		if strings.TrimSpace(a.Target) == "" || a.Size < 0 {
			return Manifest{}, fmt.Errorf("artifact target and non-negative size are required")
		}
		dg, e := ParseDigest(a.Digest)
		if e != nil {
			return Manifest{}, e
		}
		artifact := Artifact{Target: strings.TrimSpace(a.Target), Digest: dg, DigestText: dg.String(), Size: a.Size, CellManifestDigest: strings.TrimSpace(a.CellManifestDigest), Source: a.Source}
		if artifact.Source != nil {
			artifact.Source.Language = strings.TrimSpace(artifact.Source.Language)
			artifact.Source.ModulePath = strings.TrimSpace(artifact.Source.ModulePath)
			artifact.Source.ImportPath = strings.TrimSpace(artifact.Source.ImportPath)
			artifact.Source.Entrypoint = strings.TrimSpace(artifact.Source.Entrypoint)
			artifact.Source.Toolchain = strings.TrimSpace(artifact.Source.Toolchain)
			if artifact.Source.Language == "" || artifact.Source.ModulePath == "" || artifact.Source.ImportPath == "" || artifact.Source.Entrypoint == "" {
				return Manifest{}, fmt.Errorf("artifact %q source language, module_path, import_path, and entrypoint are required", artifact.Target)
			}
		}
		m.Artifacts = append(m.Artifacts, artifact)
	}
	if len(m.Artifacts) == 0 {
		return Manifest{}, fmt.Errorf("at least one artifact is required")
	}
	sort.Slice(m.Dependencies, func(i, j int) bool { return m.Dependencies[i].ID < m.Dependencies[j].ID })
	for i := 1; i < len(m.Dependencies); i++ {
		if m.Dependencies[i-1].ID == m.Dependencies[i].ID {
			return Manifest{}, fmt.Errorf("duplicate dependency %q", m.Dependencies[i].ID)
		}
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Target < m.Artifacts[j].Target })
	for i := 1; i < len(m.Artifacts); i++ {
		if m.Artifacts[i-1].Target == m.Artifacts[i].Target {
			return Manifest{}, fmt.Errorf("duplicate artifact target %q", m.Artifacts[i].Target)
		}
	}
	return m, nil
}
func validRequirement(r Requirement) bool {
	s := string(r)
	if s == "*" || s == "" {
		return true
	}
	if s[0] == '^' || s[0] == '~' {
		s = s[1:]
	}
	_, e := ParseVersion(s)
	return e == nil
}
func normalized(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func CanonicalManifest(m Manifest) ([]byte, error) {
	type art struct {
		Target             string          `json:"target"`
		Digest             string          `json:"digest"`
		Size               int64           `json:"size"`
		CellManifestDigest string          `json:"cell_manifest_digest,omitempty"`
		Source             *SourceArtifact `json:"source,omitempty"`
	}
	arts := make([]art, len(m.Artifacts))
	for i, a := range m.Artifacts {
		arts[i] = art{a.Target, a.Digest.String(), a.Size, a.CellManifestDigest, a.Source}
	}
	v := struct {
		SchemaVersion int          `json:"schema_version"`
		ID            string       `json:"id"`
		Version       string       `json:"version"`
		Dependencies  []Dependency `json:"dependencies"`
		Artifacts     []art        `json:"artifacts"`
		Provides      []string     `json:"provides"`
		Consumes      []string     `json:"consumes"`
		Capabilities  []string     `json:"capabilities"`
		FusionABI     string       `json:"fusion_abi,omitempty"`
	}{1, string(m.ID), m.Version.String(), m.Dependencies, arts, m.Provides, m.Consumes, m.Capabilities, m.FusionABI}
	return json.Marshal(v)
}
