# Unified Pulp CLI

The `pulp` executable owns both runtime startup and package lifecycle commands.
The standalone `pulp-registry` executable remains available for compatibility.

```console
pulp sync                         # resolve when needed; otherwise verify pulp.lock
pulp update                       # force a fresh resolution and replace pulp.lock
pulp inspect                      # show the locked target, roots, modules, and digests
pulp inspect -json                # emit the project and lock as JSON
pulp verify                       # verify every locked manifest and artifact
pulp refresh -manifest pulp.module.toml -blob TARGET=PATH
pulp refresh-app -manifest pulp.app.toml
pulp inspect-app -manifest pulp.app.toml
pulp inspect-app -manifest pulp.app.toml -json
pulp publish -manifest pulp.module.toml -blob TARGET=PATH
```

`inspect-app` validates the same application graph used at runtime and prints
its stable topological levels, start order, reverse stop order, dependencies,
dependents, and provider contracts without starting any cell. A cell starts
only when all of its direct dependencies are ready; multiple ready cells may
initialize concurrently. The JSON schema is
`pulp.application-dependency-plan/v1`.

`sync`, `update`, and `verify` accept `-registry` and repeatable `-remote`
options. Project remotes declared in `pulp.modules.toml` are also used by
`sync` and `update`. With no `-registry`, the normal user cache is created and
used automatically.

`pulp update` differs from `pulp sync`: update always resolves the declared
constraints again, while sync retains an existing lock that matches the roots
and passes content verification.

Deployment recovery and rollback planning read the integrity-checked durable
journal without changing a running process:

```console
pulp recovery -history data/deployments.jsonl
pulp rollback -history data/deployments.jsonl
pulp rollback -history data/deployments.jsonl -json
```

Recovery reports incomplete transactions, the last committed graph, and the
prior graph that is the safe rollback target. `-apply` currently fails closed:
although live rollback exists inside a runtime transaction, the local control
socket does not yet expose an authenticated graph-activation operation. A new
CLI process therefore never pretends to control an unrelated runtime.

Runtime invocations are unchanged:

```console
pulp -app pulp.app.toml
pulp -host pulp.host.toml
pulp -manifest pulp.cell.toml
pulp ctl status
```
