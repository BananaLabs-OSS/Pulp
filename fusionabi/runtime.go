// Package fusionabi defines the bounded in-process contract used by fused Pulp modules.
package fusionabi

import (
	"context"
	"fmt"
)

// Context is the immutable authority and identity of one logical member.
type Context struct {
	Name, Version           string
	Providers, Capabilities []string
}

// Call identifies the logical member responsible for a routed operation.
type Call struct {
	Context  context.Context
	Member   Context
	Provider string
	Payload  []byte
}

// Member is implemented by a v2 fusion entrypoint.
type Member interface {
	Init([]byte) error
	Shutdown() error
	Call(Call) ([]byte, error)
	Snapshot() ([]byte, error)
	Restore([]byte) error
}
type Registration struct {
	Context      Context
	Config       []byte
	Snapshotable bool
	Member       Member
}

// Fault attributes a failure to its logical member and operation.
type Fault struct {
	Member, Operation string
	Err               error
}

func (f *Fault) Error() string {
	return fmt.Sprintf("fusion member %q %s: %v", f.Member, f.Operation, f.Err)
}
func (f *Fault) Unwrap() error { return f.Err }

type Runtime struct {
	members     []Registration
	providers   map[string]int
	initialized int
}

func New(registrations []Registration) (*Runtime, error) {
	r := &Runtime{members: append([]Registration(nil), registrations...), providers: map[string]int{}}
	seen := map[string]bool{}
	for i, reg := range r.members {
		if reg.Context.Name == "" || reg.Member == nil || seen[reg.Context.Name] {
			return nil, fmt.Errorf("invalid or duplicate fusion member %q", reg.Context.Name)
		}
		seen[reg.Context.Name] = true
		for _, p := range reg.Context.Providers {
			if _, ok := r.providers[p]; ok {
				return nil, fmt.Errorf("duplicate fusion provider %q", p)
			}
			r.providers[p] = i
		}
	}
	return r, nil
}
func (r *Runtime) Init() error {
	for i := range r.members {
		if err := r.members[i].Member.Init(append([]byte(nil), r.members[i].Config...)); err != nil {
			fault := &Fault{r.members[i].Context.Name, "init", err}
			_ = r.shutdown(r.initialized)
			return fault
		}
		r.initialized++
	}
	return nil
}
func (r *Runtime) Shutdown() error { return r.shutdown(r.initialized) }
func (r *Runtime) shutdown(count int) error {
	var first error
	for i := count - 1; i >= 0; i-- {
		if err := r.members[i].Member.Shutdown(); err != nil && first == nil {
			first = &Fault{r.members[i].Context.Name, "shutdown", err}
		}
	}
	r.initialized = 0
	return first
}
func (r *Runtime) Call(ctx context.Context, provider string, payload []byte) ([]byte, error) {
	i, ok := r.providers[provider]
	if !ok {
		return nil, fmt.Errorf("unknown fusion provider %q", provider)
	}
	reg := r.members[i]
	out, err := reg.Member.Call(Call{ctx, reg.Context, provider, append([]byte(nil), payload...)})
	if err != nil {
		return nil, &Fault{reg.Context.Name, "call " + provider, err}
	}
	return out, nil
}
func (r *Runtime) Snapshot() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, reg := range r.members {
		if reg.Snapshotable {
			b, err := reg.Member.Snapshot()
			if err != nil {
				return nil, &Fault{reg.Context.Name, "snapshot", err}
			}
			out[reg.Context.Name] = append([]byte(nil), b...)
		}
	}
	return out, nil
}
func (r *Runtime) Restore(parts map[string][]byte) error {
	for _, reg := range r.members {
		if !reg.Snapshotable {
			continue
		}
		b, ok := parts[reg.Context.Name]
		if !ok {
			return fmt.Errorf("snapshot partition missing member %q", reg.Context.Name)
		}
		if err := reg.Member.Restore(append([]byte(nil), b...)); err != nil {
			return &Fault{reg.Context.Name, "restore", err}
		}
	}
	return nil
}
