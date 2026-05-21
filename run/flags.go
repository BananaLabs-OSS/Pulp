package run

import "strings"

// sliceFlag implements flag.Value for a repeatable string flag. The same
// flag name can appear multiple times on the command line; each occurrence
// appends to the slice. Comma-separated values in a single occurrence are
// also accepted ("-manifest a.toml,b.toml").
//
// Usage:
//
//	var manifests sliceFlag
//	flag.Var(&manifests, "manifest", "path to pulp.cell.toml (repeatable)")
//
//	// -manifest a.toml -manifest b.toml      → [a.toml, b.toml]
//	// -manifest a.toml,b.toml                → [a.toml, b.toml]
//	// -manifest a.toml                       → [a.toml]
type sliceFlag []string

func (s *sliceFlag) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *sliceFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		*s = append(*s, part)
	}
	return nil
}
