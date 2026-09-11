# Application composition identity

After `LoadApp` has validated an application, Pulp assigns a path-independent
SHA-256 identity to the complete composition. The canonical input covers:

- application schema, name, version, and digest policy;
- orchestration bytes and their validated digest;
- logical cell identity, contracts, lifecycle settings, semantic configuration,
  and actual Wasm bytes;
- expanded placement addresses, instances, and configuration; and
- selected fusion units, members, contracts, and physical Wasm artifacts.

Absolute manifest, script, and artifact paths are deliberately excluded. Two
identical verified compositions in different checkout or installation roots
therefore have the same identity. Formatting-only TOML changes also do not
change it; executable or semantic changes do.

Hosts can inspect an application before granting capabilities:

```go
identity, err := run.InspectApplicationComposition("pulp.app.toml")
```

A runtime created with `run.NewDirectApplicationRuntime` exposes the same value
through `CompositionIdentity()`. This composition identity names immutable code
and wiring; `Identity()` continues to name the independently stateful live
application instance.
