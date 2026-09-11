# Pulp module workflow

Applications declare unhashed requirements in `pulp.modules.toml`:

```toml
schema_version = 1
target = "wasm32-wasip1+pulp-v1"
remotes = ["https://registry.example.com"]

[[modules]]
id = "example/render"
requirement = "^1.2.0"
```

`pulp-registry sync` checks the automatic local cache, downloads missing
releases from those remotes, verifies every manifest and blob, and atomically
creates or refreshes `pulp.lock`. A valid unchanged lock is reused offline.

Module authors generate digests rather than editing them:

```sh
pulp-registry publish --refresh \
  -manifest pulp.module.toml \
  -blob wasm32-wasip1+pulp-v1=dist/module.wasm
```

This hashes every declared artifact, records its exact size, publishes those
exact bytes, then atomically replaces `pulp.module.toml`. `refresh` performs
only the canonical local rewrite when publication is intentionally separate.

An existing deployable cell can be published without duplicating its provider,
capability, version, ABI, or artifact declarations:

```sh
pulp-registry publish-cell \
  -registry /var/lib/pulp-registry \
  -manifest pulp.cell.toml \
  -module example/render
```

The command validates the cell, derives a canonical module manifest, and
rejects the artifact when it disagrees with the cell's optional WASM digest.

Buildable releases attach source metadata to a source artifact:

```toml
[[artifacts]]
target = "source/go"
digest = "sha256:..."
size = 1234

[artifacts.source]
language = "go"
module_path = "example/render"
import_path = "example/render/pulp"
entrypoint = "Register"
toolchain = "go1.25.13"
```

The artifact digest covers the source archive. The canonical manifest digest
covers its build metadata. Fusion resolves this record from the exact locked
release and verifies the blob before accepting it.

## Private registry

A local content-addressed registry can serve credential-free reads to trusted
private-network clients while requiring a separate credential for writes:

```sh
pulp-registry serve \
  -registry /var/lib/pulp-registry \
  -listen 10.0.0.10:8090 \
  -publish-token-file /etc/pulp-registry/publish.token \
  -publish-namespace bananalabs
```

Omitting `-publish-token-file` leaves the publication route unregistered.
Writers use `registry.HTTPPublisher` or pass `-remote` and `-token-file` to
`pulp-registry publish` / `publish-cell`; ordinary `registry.HTTPStore` readers
never carry that token. Put TLS in this process with `-tls-cert` / `-tls-key`,
or terminate it at a private reverse proxy. Clients retain verified immutable
artifacts in their automatic local cache. Production images should embed their
resolved lock and artifacts instead of depending on the private registry being
reachable during startup.
