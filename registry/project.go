package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const DefaultProjectFile = "pulp.modules.toml"
const DefaultLockFile = "pulp.lock"

// Project is the human-authored dependency declaration. Exact versions and
// content hashes belong only in pulp.lock.
type Project struct {
	SchemaVersion int
	Target        string
	Modules       []Dependency
	Remotes       []string
}

type rawProject struct {
	SchemaVersion int                                `toml:"schema_version"`
	Target        string                             `toml:"target"`
	Remotes       []string                           `toml:"remotes"`
	Modules       []struct{ ID, Requirement string } `toml:"modules"`
}

func ParseProject(data []byte) (*Project, error) {
	var raw rawProject
	meta, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("parse module project: %w", err)
	}
	if fields := meta.Undecoded(); len(fields) != 0 {
		return nil, fmt.Errorf("unknown module project field %s", fields[0])
	}
	if raw.SchemaVersion != 1 {
		return nil, fmt.Errorf("module project schema_version must be 1")
	}
	p := &Project{SchemaVersion: 1, Target: strings.TrimSpace(raw.Target), Remotes: normalized(raw.Remotes)}
	if p.Target == "" {
		return nil, fmt.Errorf("module project target is required")
	}
	seen := map[ModuleID]bool{}
	for _, module := range raw.Modules {
		id, err := ParseModuleID(module.ID)
		if err != nil {
			return nil, err
		}
		requirement := Requirement(strings.TrimSpace(module.Requirement))
		if !validRequirement(requirement) {
			return nil, fmt.Errorf("module %s has invalid requirement %q", id, requirement)
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate project module %s", id)
		}
		seen[id] = true
		p.Modules = append(p.Modules, Dependency{ID: id, Requirement: requirement})
	}
	if len(p.Modules) == 0 {
		return nil, fmt.Errorf("module project requires at least one module")
	}
	sort.Slice(p.Modules, func(i, j int) bool { return p.Modules[i].ID < p.Modules[j].ID })
	return p, nil
}

func LoadProject(path string) (*Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProject(b)
}

func ResolveProject(ctx context.Context, store Store, project *Project) (*Lock, error) {
	if project == nil {
		return nil, fmt.Errorf("module project is required")
	}
	return Resolve(ctx, store, Request{Target: project.Target, Roots: project.Modules})
}

// EnsureProjectLock verifies an existing lock or atomically replaces it by
// resolving the project. It never performs textual hash substitution.
func EnsureProjectLock(ctx context.Context, store Store, project *Project, lockPath string) (*Lock, bool, error) {
	if wire, err := os.ReadFile(lockPath); err == nil {
		if lock, parseErr := ParseLock(wire); parseErr == nil && lock.Target == project.Target && equalRoots(lock.Roots, project.Modules) {
			if verifyErr := Verify(ctx, store, lock); verifyErr == nil {
				return lock, false, nil
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	lock, err := ResolveProject(ctx, store, project)
	if err != nil {
		return nil, false, err
	}
	wire, err := lock.Marshal()
	if err != nil {
		return nil, false, err
	}
	wire = append(wire, '\n')
	if err := AtomicWrite(lockPath, wire, 0o644); err != nil {
		return nil, false, err
	}
	return lock, true, nil
}

func equalRoots(a, b []Dependency) bool {
	if len(a) != len(b) {
		return false
	}
	ac, bc := append([]Dependency(nil), a...), append([]Dependency(nil), b...)
	sort.Slice(ac, func(i, j int) bool { return ac[i].ID < ac[j].ID })
	sort.Slice(bc, func(i, j int) bool { return bc[i].ID < bc[j].ID })
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pulp-write-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = io.Copy(f, bytes.NewReader(data))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
