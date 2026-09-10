package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

type Request struct {
	Target string
	Roots  []Dependency
}
type LockedDependency struct {
	ID      ModuleID `json:"id"`
	Version string   `json:"version"`
}
type LockedModule struct {
	ID             ModuleID           `json:"id"`
	Version        string             `json:"version"`
	ManifestDigest string             `json:"manifest_digest"`
	ArtifactDigest string             `json:"artifact_digest"`
	Dependencies   []LockedDependency `json:"dependencies"`
}
type Lock struct {
	SchemaVersion int            `json:"schema_version"`
	Target        string         `json:"target"`
	Roots         []Dependency   `json:"roots"`
	Modules       []LockedModule `json:"modules"`
}

func (l Lock) Marshal() ([]byte, error) { return json.MarshalIndent(l, "", "  ") }
func ParseLock(data []byte) (*Lock, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var l Lock
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("parse lock: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("parse lock: trailing value")
	}
	if l.SchemaVersion != 1 || l.Target == "" {
		return nil, fmt.Errorf("invalid lock schema or target")
	}
	return &l, nil
}

func Resolve(ctx context.Context, s Store, r Request) (*Lock, error) {
	if r.Target == "" {
		return nil, fmt.Errorf("target is required")
	}
	constraints := map[ModuleID][]Requirement{}
	for _, root := range r.Roots {
		if _, err := ParseModuleID(string(root.ID)); err != nil {
			return nil, err
		}
		if !validRequirement(root.Requirement) {
			return nil, fmt.Errorf("invalid requirement %q", root.Requirement)
		}
		constraints[root.ID] = append(constraints[root.ID], root.Requirement)
	}
	chosen, err := solve(ctx, s, constraints, map[ModuleID]Manifest{})
	if err != nil {
		return nil, err
	}
	if err = checkCycles(chosen); err != nil {
		return nil, err
	}
	l := &Lock{SchemaVersion: 1, Target: r.Target, Roots: append([]Dependency(nil), r.Roots...)}
	sort.Slice(l.Roots, func(i, j int) bool { return l.Roots[i].ID < l.Roots[j].ID })
	ids := make([]string, 0, len(chosen))
	for id := range chosen {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, name := range ids {
		m := chosen[ModuleID(name)]
		canonical, _ := CanonicalManifest(m)
		var a *Artifact
		for i := range m.Artifacts {
			if m.Artifacts[i].Target == r.Target {
				a = &m.Artifacts[i]
				break
			}
		}
		if a == nil {
			return nil, fmt.Errorf("%s has no artifact for target %q", name, r.Target)
		}
		lm := LockedModule{ID: m.ID, Version: m.Version.String(), ManifestDigest: Sum(canonical).String(), ArtifactDigest: a.Digest.String()}
		for _, d := range m.Dependencies {
			dm := chosen[d.ID]
			lm.Dependencies = append(lm.Dependencies, LockedDependency{d.ID, dm.Version.String()})
		}
		l.Modules = append(l.Modules, lm)
	}
	return l, nil
}
func solve(ctx context.Context, s Store, c map[ModuleID][]Requirement, chosen map[ModuleID]Manifest) (map[ModuleID]Manifest, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	var unresolved ModuleID
	ids := make([]string, 0)
	for id := range c {
		if _, ok := chosen[id]; !ok {
			ids = append(ids, string(id))
		}
	}
	if len(ids) == 0 {
		return chosen, nil
	}
	sort.Strings(ids)
	unresolved = ModuleID(ids[0])
	versions, e := s.Versions(ctx, unresolved)
	if e != nil {
		return nil, fmt.Errorf("module %s: %w", unresolved, e)
	}
	for _, v := range versions {
		ok := true
		for _, r := range c[unresolved] {
			if !r.Matches(v) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		m, e := s.Manifest(ctx, ReleaseID{unresolved, v})
		if e != nil {
			continue
		}
		nc := cloneConstraints(c)
		valid := true
		for _, d := range m.Dependencies {
			nc[d.ID] = append(nc[d.ID], d.Requirement)
			if existing, yes := chosen[d.ID]; yes && !d.Requirement.Matches(existing.Version) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		ns := cloneChosen(chosen)
		ns[unresolved] = m
		if result, e := solve(ctx, s, nc, ns); e == nil {
			return result, nil
		}
	}
	return nil, fmt.Errorf("no version of %s satisfies constraints", unresolved)
}
func cloneConstraints(in map[ModuleID][]Requirement) map[ModuleID][]Requirement {
	o := map[ModuleID][]Requirement{}
	for k, v := range in {
		o[k] = append([]Requirement(nil), v...)
	}
	return o
}
func cloneChosen(in map[ModuleID]Manifest) map[ModuleID]Manifest {
	o := map[ModuleID]Manifest{}
	for k, v := range in {
		o[k] = v
	}
	return o
}
func checkCycles(ms map[ModuleID]Manifest) error {
	state := map[ModuleID]uint8{}
	stack := []ModuleID{}
	var visit func(ModuleID) error
	visit = func(id ModuleID) error {
		if state[id] == 1 {
			return fmt.Errorf("dependency cycle involving %v", append(stack, id))
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		for _, d := range ms[id].Dependencies {
			if e := visit(d.ID); e != nil {
				return e
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	ids := []string{}
	for id := range ms {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		if e := visit(ModuleID(id)); e != nil {
			return e
		}
	}
	return nil
}
func Verify(ctx context.Context, s Store, l *Lock) error {
	if l == nil || l.SchemaVersion != 1 {
		return fmt.Errorf("invalid lock")
	}
	locked := make(map[ModuleID]LockedModule, len(l.Modules))
	for i, lm := range l.Modules {
		if _, err := ParseModuleID(string(lm.ID)); err != nil {
			return err
		}
		if i > 0 && l.Modules[i-1].ID >= lm.ID {
			return fmt.Errorf("locked modules are not in canonical order")
		}
		if _, exists := locked[lm.ID]; exists {
			return fmt.Errorf("duplicate locked module %s", lm.ID)
		}
		locked[lm.ID] = lm
	}
	for _, root := range l.Roots {
		lm, ok := locked[root.ID]
		if !ok {
			return fmt.Errorf("locked root %s is missing", root.ID)
		}
		v, err := ParseVersion(lm.Version)
		if err != nil || !validRequirement(root.Requirement) || !root.Requirement.Matches(v) {
			return fmt.Errorf("locked root %s does not satisfy %q", root.ID, root.Requirement)
		}
	}
	for _, lm := range l.Modules {
		v, e := ParseVersion(lm.Version)
		if e != nil {
			return e
		}
		m, e := s.Manifest(ctx, ReleaseID{lm.ID, v})
		if e != nil {
			return e
		}
		canonical, _ := CanonicalManifest(m)
		if Sum(canonical).String() != lm.ManifestDigest {
			return fmt.Errorf("manifest digest mismatch for %s", lm.ID)
		}
		if len(lm.Dependencies) != len(m.Dependencies) {
			return fmt.Errorf("dependency lock mismatch for %s", lm.ID)
		}
		for i, dep := range m.Dependencies {
			got := lm.Dependencies[i]
			child, ok := locked[dep.ID]
			if !ok || got.ID != dep.ID || got.Version != child.Version {
				return fmt.Errorf("dependency lock mismatch for %s", lm.ID)
			}
			childVersion, err := ParseVersion(child.Version)
			if err != nil || !dep.Requirement.Matches(childVersion) {
				return fmt.Errorf("dependency constraint mismatch for %s", lm.ID)
			}
		}
		d, e := ParseDigest(lm.ArtifactDigest)
		if e != nil {
			return e
		}
		matchedTarget := false
		for _, a := range m.Artifacts {
			if a.Target == l.Target && a.Digest == d {
				matchedTarget = true
				break
			}
		}
		if !matchedTarget {
			return fmt.Errorf("locked artifact mismatch for %s target %s", lm.ID, l.Target)
		}
		rc, e := s.Blob(ctx, d)
		if e != nil {
			return e
		}
		b, e := io.ReadAll(rc)
		ce := rc.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
		if Sum(b) != d {
			return fmt.Errorf("artifact digest mismatch for %s", lm.ID)
		}
	}
	return nil
}
