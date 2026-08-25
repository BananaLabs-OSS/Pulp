# Pulp fusion

Pulp separates a logical cell (identity, providers, configuration, storage
scope, and restart policy) from its physical execution unit.

```toml
[execution]
mode = "fusible"
group = "evolution-sqlite-core"
abi = "pulp-linear-v1"
```

`fusible` is an opt-in request, not permission to weaken a boundary. At
application startup Pulp computes the requested groups and leaves a member
isolated when the group has fewer than two cells or its members differ in
capabilities, memory limit, call timeout, restart policy, or thread policy.
The fallback preserves the public `provides` / `consumes` ABI exactly.

## Artifact backend contract

An accepted group becomes one physical instance only when the Pulp fusible
compiler emits an artifact for the exact group digest. That compiler must:

1. compile source packages against `pulp-linear-v1`, rather than concatenate
   independently compiled Wasm binaries;
2. retain a logical-cell dispatch table so `pulp.call("logical-cell", ...)`
   remains stable;
3. preserve each logical cell's capability and storage scope inside the fused
   instance; a shared module must not turn one cell's host authority into
   ambient authority for another;
4. restart, trace, meter, and snapshot logical cells independently wherever
   their manifest asks for it; and
5. fall back to isolated modules when a matching verified fused artifact is
   absent.

This is why a source-level Pulp ABI is required for full shared-memory
benefits. A generic `.wasm` component remains deployable through the normal
isolated path.

## Workspace candidates

The workspace contains Pulp/Fiber source cells suitable for conversion to the
fusible build ABI. The first opted-in groups are:

- `bananagine-state`: registry + template catalog (no host capabilities);
- `sessions-state-engine`: Sessions Lua's three durable domain providers;
- `bananapulse-sqlite-core`: auth, monitor, and subscriber owners; and
- `evolution-sqlite-core`: the SQLite-only Evolution domain owners.

Privileged façades, HTTP entrances, Docker/worker cells, S3 cells, and cells
with different capability sets deliberately remain separate until a logical
scope-aware artifact backend can prove their host-effect boundaries.

`Bananagine/state-cell` is the first emitted artifact: it compiles the
registry and template-catalog source owners into one `bananagine-state.wasm`
and retains their original provider names. The production Bananagine
composition, generic production image recipe, and game-server rollout gate
all build, pin, and address that one physical cell for both logical surfaces.

`Sessions-Gene/state-engine` is the second emitted artifact. It compiles the
identity-retention, order-config, and provisioning-failure provider libraries
into one private `sessions-state-engine.wasm`; Sessions Lua remains the
resolver and calls that one state target. The original logical owner wrappers
remain available for isolated development and debugging. Packaging their old
Wasm binaries together would have retained separate heaps and would not have
satisfied this contract.
