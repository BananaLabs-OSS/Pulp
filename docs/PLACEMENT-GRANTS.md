# Host-owned placement grants

`ext.PlacementGrant` describes authority for one immutable `ext.Scope` and one
capability. Grants name a resource, normalized rights, and host-only adapter
attributes. They are deployment input, never guest manifest configuration.

`PlacementGrantResolver` is read-only. `StaticPlacementGrants` validates,
normalizes, clones, and indexes grants by the complete application instance,
cell, and cell-instance scope plus capability. Missing grants fail lookup; no
parent, sibling, or capability fallback is implied. A nil resolver preserves
legacy extension behavior during migration.

Hosts supply the resolver through `DirectApplicationOptions.PlacementGrants` or
`HostRuntimeOptions.PlacementGrants`. Pulp carries it through
`ScopedApplicationRuntimeFactoryConfig` into every declared capability's
`SetupEnv`. One resolver may contain grants for multiple applications and cell
placements, but an adapter must query only the exact cell scope it is binding.
Pulp never derives placement grants from application or cell manifests.

Adapters must resolve with the scope obtained from `ext.ValidatedScopeOf(cell)`
at registration, enforce the returned rights in every bound operation, and
treat attributes as host configuration. Filesystem adapters should canonicalize
the granted root and enforce `read`/`list`/`stat` separately from mutation.
Process adapters should bind an exact executable identity and canonical working
root per scope. Neither adapter should accept guest-selected absolute paths.
