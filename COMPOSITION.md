# Pulp composition contracts

Pulp modules are sealed implementation units. They do not import or directly
call one another. Lua owns application composition and routes values between
versioned semantic contracts.

Contracts use a stable `.vN` identity and one of five kinds: `command`,
`event`, `query`, `state`, or `lifecycle`. Every contract has exactly one
authoritative owner. Other modules declare consumption; the existing Pulp
capability boundary denies undeclared calls.

```text
engine module ──versioned event──▶ Lua ──versioned command──▶ game module
game module   ──versioned event──▶ Lua ──versioned command──▶ engine module
```

Fusion is a deployment decision below this graph. Logical names, contracts,
ownership, and Lua-visible routing do not change merely because several
implementations share one physical Wasm artifact.

The `composition` Go package validates new contract catalogs. Existing
applications retain their legacy exact-string contracts until migrated, so
adopting this layer does not silently reinterpret established providers.
