# Pulp checkpoint — 2026-09-08

## Current outcome

Pulp now has an executable vertical slice connecting module resolution,
hermetic fusion, the Registry, AI-governed deployment, live state transfer and
rollback.

The golden demonstration builds against the real Fiber checkout. It does not
use the former Fiber-compatible ABI fixture.

```text
real Fiber source
+ pinned upstream Go sources
+ sample logical modules
        |
        v
ABI-v2 fusion recipe and complete source attestation
        |
        v
offline Go/WASI build (GOPROXY=off)
        |
        v
fused reactor initialization and shutdown through Fiber
        |
        v
signed Registry publication, resolution and verification
        |
        v
approved live upgrade, state restoration and unhealthy rollback
```

## Completed capabilities

- Dependency DAG validation and parallel lifecycle ordering.
- Local, cached, remote and hosted Registry behavior.
- Immutable, content-addressed packages and exact dependency locks.
- Accountless Ed25519 publication and approval identities.
- Public fusion SDK independent of Pulp's internal manifest types.
- ABI-v2 fused member lifecycle, providers, configuration and snapshots.
- Deterministic fusion recipes and build attestations.
- Go ecosystem importer preserving upstream identity and native checksums.
- Persistent `pulp.go-sources.lock.json` plus verified canonical archives.
- Deterministic packaging of first-party/local Go module directories.
- Offline transitive dependency materialization through Go `replace` entries.
- Unified `pulp import-go` command for intentional dependency refreshes.
- Atomic runtime routing, stateful live reconciliation and health rollback.
- Integrity-chained deployment recovery history.
- Revision-bound AI ChangeSets, evidence, policy and signed approval.
- MCP adapter for explanatory/tool-facing AI integration.
- Automatic application Lua digest refresh support.

## Golden proof

Repository:

`$WORKSPACE/Pulp-deployment-demo`

Run:

```bash
env GOCACHE=/tmp/demo-go-cache \
    GOMODCACHE=/tmp/demo-go-mod-cache \
    go test -race ./... -count=1
```

The proof currently verifies:

1. Fiber's seven upstream modules reopen from a persisted source lock.
2. Every source archive is checked against its SHA-256.
3. The fusion builder receives no dependency network access.
4. Real Fiber and two logical modules compile into a Go/WASI reactor.
5. The reactor initializes and shuts down through Fiber's real exports.
6. The fused artifact and attestation are signed and published.
7. Registry resolution and complete digest verification succeed.
8. An approved `1.0.0` to `1.1.0` live upgrade preserves state.
9. An unhealthy `1.2.0` candidate is rejected without losing state.
10. Deployment recovery reports a committed graph and no incomplete work.

## Dependency refresh

Resolution is intentionally separate from builds. Refresh Fiber's upstream
source lock only when changing dependencies:

```bash
pulp import-go \
  -go-mod ../Fiber/go.mod \
  -go-sum ../Fiber/go.sum \
  -out ../Pulp-deployment-demo/golden/testdata/fiber-go-sources \
  -proxy https://proxy.golang.org
```

Normal builds reopen the saved lock and remain offline.

## Validation at this checkpoint

The following passed with the race detector and `go vet`:

- `Pulp/fusion`
- `Pulp/internal/fusion`
- `Pulp/registry`
- `Pulp/internal/pulpcli`
- the complete `Pulp-deployment-demo` golden suite

The larger workspace still contains independent integration fixtures affected
by missing sibling repositories, sandbox networking, actively changing
Evolution/resolver digests and evolving Bananauth contract expectations. Those
are not failures of the golden fusion path and were deliberately not hidden or
rewritten during this work.

## Best next starting point

Turn the demonstrated APIs into the ordinary project workflow:

1. Add a declarative source/import section to the Pulp project manifest.
2. Make `pulp sync` refresh or reuse the Go source lock as appropriate.
3. Let `pulp build` select fusion groups and invoke the public fusion SDK.
4. Let `pulp publish` sign and publish the resulting fused artifact and
   attestation without custom Go code.
5. Add a CI profile separating hermetic core/golden tests from optional
   cross-repository and network integration tests.

That is packaging and productization of a working architecture, rather than a
new architectural experiment.
