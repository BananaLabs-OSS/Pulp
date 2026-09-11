# Go ecosystem source locks

Pulp separates network-capable dependency resolution from hermetic fusion.

```text
go.mod + go.sum
        |
        v
fusion.GoImporter (network may be enabled)
        |
        v
pulp.go-sources.lock.json + archives/<sha256>.zip
        |
        v
fusion.OpenGoSourceLock (offline and digest-verifying)
        |
        v
fusion.Build with GOPROXY=off
```

`GoImporter` delegates version and native checksum verification to the Go
toolchain. Pulp then canonicalizes each downloaded module archive and records:

- its original module path and version;
- its Go source and `go.mod` checksums;
- its upstream/proxy provenance;
- the SHA-256 of the exact canonical source archive used for fusion.

The imported dependencies are included in the canonical fusion recipe. Any
change to a version, checksum, origin, or source archive therefore changes the
recipe digest and build attestation.

Importing does not claim ownership of an upstream package. It creates a
verified, content-addressed cache of that package under its original identity.

## API outline

```go
lock, err := (fusion.GoImporter{Proxy: "https://proxy.golang.org"}).Import(
    ctx,
    goMod,
    goSum,
)
if err != nil { /* handle */ }

if err := lock.Save(".pulp/go-sources"); err != nil { /* handle */ }

locked, err := fusion.OpenGoSourceLock(".pulp/go-sources")
if err != nil { /* tampering, missing archive, or invalid lock */ }

result, err := fusion.Build(ctx, fusion.Request{
    // Direct members and the verified Fiber source are omitted here.
    Dependencies: locked.Dependencies,
    Archives: fusion.ArchiveProviders{
        directRegistrySources,
        locked,
    },
})
```

Import should happen when dependencies are intentionally refreshed. Normal
builds and deployments reopen the saved lock and do not contact the network.

The unified CLI performs that refresh explicitly:

```bash
pulp import-go \
  -go-mod ../Fiber/go.mod \
  -go-sum ../Fiber/go.sum \
  -out .pulp/fiber-go-sources \
  -proxy https://proxy.golang.org
```

First-party or locally developed modules such as Fiber can be bound with
`fusion.PackGoModuleDirectory`; this produces the same deterministic archive
shape without representing that source as an upstream dependency.
