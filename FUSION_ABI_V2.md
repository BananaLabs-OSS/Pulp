# Pulp fusion member ABI v2

`pulp-member-v2` preserves logical member boundaries inside one physical
fusion unit. It is opt-in; v1 artifacts and manifests keep their existing
shared-config behavior.

Each registered member has immutable identity, version, provider ownership,
declared capabilities, its own configuration, and a snapshotable flag. The
fusion runtime provides:

- ordered initialization with reverse-order rollback and shutdown;
- provider-to-member routing with logical identity attached to each call;
- errors attributed to member and operation;
- snapshots partitioned by logical member name;
- exact duplicate-member and duplicate-provider rejection.

The planner permits different configurations and snapshotable members only
for the exact `pulp-member-v2` ABI. Capability sets must still be equal because
today's Wasm host binds capabilities to the physical instance. This prevents
one member from receiving another member's ambient host authority.

The reusable host-side Go contract is the public `fusionabi` package. Source
modules expose the build ABI below (function values keep the generated wrapper
independent of a concrete module implementation):

```go
func RegisterV2(name, version string, providers, capabilities []string) (
    init func(config []byte) error,
    shutdown func() error,
    call func(provider string, payload []byte) ([]byte, error),
    snapshot func() ([]byte, error),
    restore func([]byte) error,
    err error,
)
```

The generated wrapper connects providers through `pulp.Provide` and lifecycle
through `pulp.OnInit`/`pulp.OnShutdown`. Configuration comes from the signed
recipe member scope, never the physical cell's shared init payload.

## Fail-closed boundary

An artifact claiming v2 must carry v2 recipe/attestation member scopes before
selection. V1 `Register` entrypoints are rejected from v2 recipes. Snapshot
and restore hooks are retained per member; host-triggered persistence still
requires dedicated snapshot exports in Fiber/Pulp. Physical Wasm memory
remains one failure domain, so v2 attribution cannot survive corruption or
failure of the shared Wasm instance.
