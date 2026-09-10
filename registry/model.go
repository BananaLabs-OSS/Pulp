// Package registry implements Pulp's local-first, content-addressed module registry.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var moduleIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._/-]*[a-z0-9])?$`)

type ModuleID string

func ParseModuleID(s string) (ModuleID, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 255 || !moduleIDPattern.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") {
		return "", fmt.Errorf("invalid module id %q", s)
	}
	return ModuleID(s), nil
}

type Digest [sha256.Size]byte

func Sum(b []byte) Digest       { return sha256.Sum256(b) }
func (d Digest) String() string { return "sha256:" + hex.EncodeToString(d[:]) }
func ParseDigest(s string) (Digest, error) {
	var d Digest
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "sha256:") {
		return d, fmt.Errorf("digest must use sha256: prefix")
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	if err != nil || len(b) != sha256.Size {
		return d, fmt.Errorf("invalid sha256 digest %q", s)
	}
	copy(d[:], b)
	return d, nil
}

type Version struct {
	Major, Minor, Patch uint64
	Pre                 string
}

func ParseVersion(s string) (Version, error) {
	var v Version
	s = strings.TrimSpace(strings.TrimPrefix(s, "v"))
	main := s
	if strings.Contains(main, "+") {
		return v, fmt.Errorf("version %q: build metadata is not supported", s)
	}
	if i := strings.IndexByte(main, '-'); i >= 0 {
		v.Pre, main = main[i+1:], main[:i]
		if v.Pre == "" {
			return v, fmt.Errorf("invalid version %q", s)
		}
		for _, identifier := range strings.Split(v.Pre, ".") {
			if identifier == "" {
				return v, fmt.Errorf("invalid version %q", s)
			}
			for _, ch := range identifier {
				if !(ch == '-' || ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z') {
					return v, fmt.Errorf("invalid version %q", s)
				}
			}
			if len(identifier) > 1 && identifier[0] == '0' {
				if _, err := strconv.ParseUint(identifier, 10, 64); err == nil {
					return v, fmt.Errorf("invalid numeric prerelease in %q", s)
				}
			}
		}
	}
	p := strings.Split(main, ".")
	if len(p) != 3 {
		return v, fmt.Errorf("version %q must be major.minor.patch", s)
	}
	vals := []*uint64{&v.Major, &v.Minor, &v.Patch}
	for i := range p {
		if p[i] == "" || (len(p[i]) > 1 && p[i][0] == '0') {
			return v, fmt.Errorf("invalid version %q", s)
		}
		n, err := strconv.ParseUint(p[i], 10, 64)
		if err != nil {
			return v, fmt.Errorf("invalid version %q", s)
		}
		*vals[i] = n
	}
	return v, nil
}
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
func (v Version) Compare(o Version) int {
	if v.Major != o.Major {
		if v.Major < o.Major {
			return -1
		}
		return 1
	}
	if v.Minor != o.Minor {
		if v.Minor < o.Minor {
			return -1
		}
		return 1
	}
	if v.Patch != o.Patch {
		if v.Patch < o.Patch {
			return -1
		}
		return 1
	}
	if v.Pre == o.Pre {
		return 0
	}
	if v.Pre == "" {
		return 1
	}
	if o.Pre == "" {
		return -1
	}
	a, b := strings.Split(v.Pre, "."), strings.Split(o.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		an, ae := strconv.ParseUint(a[i], 10, 64)
		bn, be := strconv.ParseUint(b[i], 10, 64)
		if ae == nil && be == nil {
			if an < bn {
				return -1
			}
			return 1
		}
		if ae == nil {
			return -1
		}
		if be == nil {
			return 1
		}
		return strings.Compare(a[i], b[i])
	}
	if len(a) < len(b) {
		return -1
	}
	return 1
}

type Requirement string

func (r Requirement) Matches(v Version) bool {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "*" {
		return v.Pre == ""
	}
	if strings.HasPrefix(s, "^") {
		b, e := ParseVersion(s[1:])
		if e != nil {
			return false
		}
		upper := Version{Major: b.Major + 1}
		if b.Major == 0 {
			upper = Version{Minor: b.Minor + 1}
			if b.Minor == 0 {
				upper = Version{Patch: b.Patch + 1}
			}
		}
		return v.Compare(b) >= 0 && v.Compare(upper) < 0 && v.Pre == ""
	}
	if strings.HasPrefix(s, "~") {
		b, e := ParseVersion(s[1:])
		return e == nil && v.Compare(b) >= 0 && v.Compare(Version{Major: b.Major, Minor: b.Minor + 1}) < 0 && v.Pre == ""
	}
	b, e := ParseVersion(s)
	return e == nil && v.Compare(b) == 0
}

type ReleaseID struct {
	ID      ModuleID
	Version Version
}

func (r ReleaseID) String() string { return string(r.ID) + "@" + r.Version.String() }

type Dependency struct {
	ID          ModuleID    `toml:"id" json:"id"`
	Requirement Requirement `toml:"requirement" json:"requirement"`
}
type Artifact struct {
	Target             string `toml:"target" json:"target"`
	Digest             Digest `toml:"-" json:"-"`
	DigestText         string `toml:"digest" json:"digest"`
	Size               int64  `toml:"size" json:"size"`
	CellManifestDigest string `toml:"cell_manifest_digest" json:"cell_manifest_digest,omitempty"`
	// Source is present for build-input artifacts. Its fields are covered by
	// the canonical manifest digest and the artifact digest covers the source
	// archive bytes, so fusion never trusts a mutable checkout path.
	Source *SourceArtifact `toml:"source" json:"source,omitempty"`
}

// SourceArtifact describes how a verified source archive participates in a
// build. Version is inherited from the containing module release.
type SourceArtifact struct {
	Language   string `toml:"language" json:"language"`
	ModulePath string `toml:"module_path" json:"module_path"`
	ImportPath string `toml:"import_path" json:"import_path"`
	Entrypoint string `toml:"entrypoint" json:"entrypoint"`
	Toolchain  string `toml:"toolchain" json:"toolchain,omitempty"`
}
type Manifest struct {
	SchemaVersion                    int
	ID                               ModuleID
	Version                          Version
	Dependencies                     []Dependency
	Artifacts                        []Artifact
	Provides, Consumes, Capabilities []string
	FusionABI                        string
}
