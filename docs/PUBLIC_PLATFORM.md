# Public Pulp platform map

Pulp is a composition runtime, not one monolithic engine binary. The logical
system is spread across small repositories so that host capabilities, policy,
confinement, orchestration, reusable engines, and semantic compilation can be
audited independently.

## Runtime spine

- `Pulp` owns manifests, dependency planning, cell lifecycle, routing,
  placement, registry resolution, snapshots, reconciliation, and fusion.
- `Fiber` owns language-neutral contracts used by applications and effects.
- `Pulp-Lua` provides the sandboxed composition identity. Modules do not need
  to import one another; Lua routes values between declared contracts.
- `Seme` lifts supported source-language meaning into a canonical graph and
  lowers it to Wasm. Seme is optional: hand-authored or conventionally compiled
  Wasm remains valid Pulp input.

## Host capability extensions

The core host is intentionally small. Capabilities are registered by separate
Go modules, including HTTP, OAuth, JWT, SQLite, filesystem, entropy, UDP, TCP,
workers, process execution, PTY, toolchain acquisition, mDNS, and outbound
WebSocket support. A cell receives only capabilities declared by its manifest.

## Security boundary

- `Pulp-grants` owns time-bounded and revocable grants.
- `Pulp-ext-fuse` implements the filesystem policy floor on supported Linux
  hosts.
- `Pulp-ext-egress` implements network egress policy and mediation.
- `Pulp-ext-confine` launches confined workloads using platform facilities.
- `Pulp-cage` composes those mechanisms into the agent/workload cage.

These repositories are security mechanisms, not a claim that Wasm alone is a
complete operating-system sandbox. Platform-specific enforcement and its
limitations are part of the review surface.

## Registry and fusion

The native registry bootstrap stores immutable manifests and content-addressed
artifacts. Applications resolve requirements into an exact lock. Production
can carry the locked artifacts and start without registry availability.

Fusion is a physical deployment optimization below the logical graph. A fused
artifact must attest to the exact member releases, recipe, providers,
capabilities, and output digest. Logical identities and Lua-visible routing do
not disappear when modules share one Wasm instance. Isolated fallback is
explicit rather than silent.

## Reusable engines

`pulp-engines` contains broken-out logical engines and their deployable cells.
The registry catalog should publish these logical releases as the primary
units. Fused state engines are additional derived deployment artifacts, not
replacements for their component releases.

## Reproducing the workspace

Run `scripts/bootstrap-public-workspace.sh <directory>` from a Pulp checkout.
It clones the sibling layout used by the current Go modules. Then run
`scripts/audit-public-workspace.sh <directory>` for formatting, vet, and test
gates. Platform-sensitive confinement tests may require Linux user namespaces,
FUSE, and TUN support; a skip or failure must be reported rather than silently
treated as a passing confinement proof.

## Maturity statement

This is active pre-1.0 infrastructure. The registry, composition graph,
capability isolation, deterministic Lua wire format, placement planning,
snapshots, live reconciliation, and fusion pipeline have implementations and
tests. Cross-platform confinement, complete automatic fusion, browser hosting,
and the Seme language surface remain evolving. Read source and tests as the
authority; dated checkpoint documents are historical evidence, not the current
contract.
