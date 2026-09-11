# Automatic fusion runtime boundary

Application startup can receive a `run.FusionRuntimePreparer`. Deployment
binaries pass it through `run.MainWithOptions`; programmatic direct and hosted
runtimes use `DirectApplicationOptions.Fusion` and `HostRuntimeOptions.Fusion`.
Before setting
up host capabilities or loading any guest, the runtime builds the conservative
fusion plan and asks the preparer for a verified activation for each eligible
group. A fused activation becomes a physical execution unit. A preparation
error stops startup; isolated fallback only occurs when the activation carries
an explicit fallback cause.

Manual `[[execution_units]]` continue to work and take precedence over an
automatic activation for the same complete group. Partial or conflicting
physical ownership is rejected.

## Boundaries preserved by ABI v2

- Logical names and provider routing remain visible to Lua and sibling calls.
- Registry selection verifies the exact members, providers, capabilities,
  recipe, source releases, and Wasm digest before activation.
- All members must declare the same capability set, restart policy, memory
  limit, call timeout, and configuration. Dedicated-thread and snapshotable
  cells remain isolated.
- Physical startup participates in the ordinary dependency DAG, rollback, and
  reverse-order shutdown.
- Logs identify both the source fusion group and activated physical unit.

## Exact remaining ABI v2 limitations

The current generated Go aggregator owns one Wasm instance. ABI v2 retains
member-specific configuration, lifecycle callbacks, snapshot partitions, and
error attribution, but physical memory and crash/restart isolation remain
shared by definition. Host capability imports are scoped to the physical cell;
equal capability sets prevent authority broadening. Metrics and traps can
identify member errors where the ABI reports them, while a process-level trap
still identifies the physical artifact and source group.

Stronger independent memory and crash containment requires isolated execution;
it cannot be added to a fused Wasm instance without giving up the memory-saving
property fusion exists to provide.
