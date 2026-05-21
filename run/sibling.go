// Sibling-call capability — enables in-process, direct cell-to-cell
// calls without going over HTTP. Cell B declares `consumes = ["foo"]`,
// cell A declares `provides = ["foo"]` and exports `pulp_on_call`.
// The host routes `pulp_call("A", "foo", args)` from B straight into
// A's pulp_on_call, serialized against A's own step loop via a module
// mutex so wazero never sees concurrent calls into the same module.
//
// The capability is always bound (no manifest declaration required) —
// every cell can call sibling providers as long as its manifest
// declares the target in `consumes`. Enforcement is manifest-level, not
// runtime-gated, so the call is as cheap as a Go function call plus
// two msgpack hops.

package run

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// siblingRegistry is the narrow view of the runtime the sibling-call
// handler needs: lookup a cell by name and verify the caller is
// allowed to reach it per manifest `consumes`.
type siblingRegistry struct {
	runtimes map[string]*cellRuntime
}

func newSiblingRegistry(runtimes map[string]*cellRuntime) *siblingRegistry {
	return &siblingRegistry{runtimes: runtimes}
}

// siblingCapability returns a Capability that binds the pulp_call host
// import. The registry is captured at binding time so every cell
// sees the same view of the cell set.
//
// The host-function body is wrapped in panic recovery so a misbehaving
// target cell can't take down the caller's step goroutine via a trap
// propagated through wazero. A recovered panic surfaces to the caller
// as error code 99 ("host panic") so cell authors can distinguish it
// from a normal error return.
func siblingCapability(reg *siblingRegistry) ext.Capability {
	return ext.Capability{
		Name: "pulp.sibling", // always-bound; declaring in manifest is optional
		Register: func(b wazero.HostModuleBuilder, cell ext.Cell) error {
			caller := cell.Name()
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module,
					targetPtr, targetLen,
					namePtr, nameLen,
					argsPtr, argsLen,
					respPtrOut, respLenOut uint32,
				) (rc uint32) {
					defer func() {
						if r := recover(); r != nil {
							slog.Default().Error("pulp_call host panic",
								"caller", caller,
								"panic", r,
								"stack", string(debug.Stack()),
							)
							rc = 99
						}
					}()
					if targetLen == 0 || nameLen == 0 {
						return 1
					}
					targetBytes, ok := m.Memory().Read(targetPtr, targetLen)
					if !ok {
						return 2
					}
					nameBytes, ok := m.Memory().Read(namePtr, nameLen)
					if !ok {
						return 2
					}
					var args []byte
					if argsLen > 0 {
						args, ok = m.Memory().Read(argsPtr, argsLen)
						if !ok {
							return 2
						}
						// Copy — we drop the lock on caller's memory before
						// the target is invoked.
						buf := make([]byte, len(args))
						copy(buf, args)
						args = buf
					}
					target := string(targetBytes)
					funcName := string(nameBytes)

					// Permission check against the caller's manifest.
					if !allowedToCall(reg, caller, target, funcName) {
						return 11 // "not allowed"
					}

					resp, err := reg.callDirect(ctx, caller, target, funcName, args)
					if err != nil {
						return 4
					}
					return writeSiblingResponse(ctx, m, resp, respPtrOut, respLenOut)
				}).
				Export("pulp_call")
			return nil
		},
		Stub: func(b wazero.HostModuleBuilder, _ ext.Cell) error {
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _, _, _, _, _, _, _ uint32) uint32 {
					return 99
				}).
				Export("pulp_call")
			return nil
		},
	}
}

// allowedToCall checks the caller's manifest spec against the target.
// Separated from siblingRegistry so the capability closure can call it
// without generic-interface gymnastics.
func allowedToCall(reg *siblingRegistry, caller, target, funcName string) bool {
	callerRT, ok := reg.runtimes[caller]
	if !ok {
		return false
	}
	targetRT, ok := reg.runtimes[target]
	if !ok {
		return false
	}
	for _, d := range callerRT.spec.DependsOn {
		if d == target {
			return true
		}
	}
	for _, c := range callerRT.spec.Consumes {
		if c == target {
			return true
		}
		for _, prov := range targetRT.spec.Provides {
			if c == prov {
				return true
			}
		}
	}
	_ = funcName
	return false
}

// callDirect bypasses the interface-based siblingRegistry.call path
// and invokes the target cell's Call method directly. Used from the
// pulp_call host-function closure.
func (r *siblingRegistry) callDirect(ctx context.Context, caller, target, funcName string, args []byte) ([]byte, error) {
	targetRT, ok := r.runtimes[target]
	if !ok {
		return nil, fmt.Errorf("unknown target cell %q", target)
	}
	if targetRT.failed || targetRT.cell == nil {
		return nil, fmt.Errorf("target cell %q is not running", target)
	}
	_ = caller
	return targetRT.cell.Call(ctx, funcName, args)
}

// writeSiblingResponse allocates a buffer in caller-module memory via
// the caller's pulp_alloc, writes resp, and stores (ptr, len) into the
// caller-supplied out-params.
func writeSiblingResponse(ctx context.Context, m api.Module, resp []byte, respPtrOut, respLenOut uint32) uint32 {
	var ptr uint32
	if len(resp) > 0 {
		allocFn := m.ExportedFunction("pulp_alloc")
		if allocFn == nil {
			return 7
		}
		res, err := allocFn.Call(ctx, uint64(len(resp)))
		if err != nil || len(res) == 0 {
			return 7
		}
		ptr = uint32(res[0])
		if ptr == 0 {
			return 7
		}
		if !m.Memory().Write(ptr, resp) {
			return 8
		}
	}
	if !m.Memory().WriteUint32Le(respPtrOut, ptr) {
		return 8
	}
	if !m.Memory().WriteUint32Le(respLenOut, uint32(len(resp))) {
		return 8
	}
	return 0
}

// validateSiblingLinks walks every cell's consumes + depends_on and
// confirms at least one cell in the set provides it (or is named that,
// for depends_on). Returns the list of unsatisfied references — empty
// slice means everything is wired up.
func validateSiblingLinks(runtimes map[string]*cellRuntime) []string {
	// Collect all provides by name → slice of provider cell names.
	provided := map[string][]string{} // capability/provider → cell names
	names := map[string]bool{}
	for _, rt := range runtimes {
		names[rt.spec.Name] = true
		for _, p := range rt.spec.Provides {
			provided[p] = append(provided[p], rt.spec.Name)
		}
	}
	var missing []string
	for _, rt := range runtimes {
		for _, dep := range rt.spec.DependsOn {
			if !names[dep] {
				missing = append(missing, fmt.Sprintf("%s depends_on %s (no such cell)", rt.spec.Name, dep))
			}
		}
		for _, c := range rt.spec.Consumes {
			if names[c] {
				continue
			}
			if _, ok := provided[c]; ok {
				continue
			}
			missing = append(missing, fmt.Sprintf("%s consumes %s (no provider)", rt.spec.Name, c))
		}
	}
	return missing
}

