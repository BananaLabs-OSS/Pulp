#!/bin/sh
set -eu

workspace=${1:-$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)}

modules=$(find "$workspace" -mindepth 2 -maxdepth 4 -name go.mod -print | sort)
for module in $modules; do
  repository=$(dirname "$module")
  printf '\n==> %s\n' "${repository#"$workspace"/}"
  (
    cd "$repository"
    tracked_go=$(git ls-files '*.go')
    if [ -n "$tracked_go" ]; then
      unformatted=$(printf '%s\n' "$tracked_go" | xargs gofmt -l)
      if [ -n "$unformatted" ]; then
        printf 'Unformatted Go files:\n%s\n' "$unformatted" >&2
        exit 1
      fi
    fi
    go vet ./...
    go test ./...
  )
done

registry_bin=$(mktemp "${TMPDIR:-/tmp}/pulp-registry.XXXXXX")
trap 'rm -f -- "$registry_bin"' EXIT HUP INT TERM
(cd "$workspace/Pulp" && go build -o "$registry_bin" ./cmd/pulp-registry)
(
  cd "$workspace/pulp-engines"
  PULP_REGISTRY_BIN="$registry_bin" ./registry/publish-state-engines.sh \
    --check --catalog registry/public-core-engines.tsv
)

printf '\nPublic workspace gates passed.\n'
