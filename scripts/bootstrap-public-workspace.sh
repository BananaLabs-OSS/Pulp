#!/bin/sh
set -eu

destination=${1:-pulp-platform}
organization=${PULP_GITHUB_ORG:-BananaLabs-OSS}

mkdir -p "$destination"
cd "$destination"

repositories="
Fiber
Pulp
Pulp-Lua
Pulp-ext-http
Pulp-ext-oauth
Pulp-ext-jwt
Pulp-ext-docker
Pulp-ext-gin
Pulp-ext-postgres
Pulp-ext-s3
Pulp-ext-stripe
Pulp-ext-sqlite
Pulp-ext-entropy
Pulp-ext-fs
Pulp-ext-udp
Pulp-ext-workers
Pulp-ext-process
Pulp-ext-pty
Pulp-ext-toolchain
Pulp-ext-wsout
Pulp-ext-acquire
Pulp-ext-mdns
Pulp-ext-tcp
Pulp-grants
Pulp-ext-fuse
Pulp-ext-egress
Pulp-ext-confine
Pulp-ext-hook
Pulp-cage
pulp-engines
Seme
Seme-Go-Corpus
"

for repository in $repositories; do
  if [ -d "$repository/.git" ]; then
    printf '%s already present\n' "$repository"
    continue
  fi
  git clone "https://github.com/$organization/$repository.git" "$repository"
done

printf '\nPulp public workspace is available at %s\n' "$(pwd)"
printf 'Run Pulp/scripts/audit-public-workspace.sh %s\n' "$(pwd)"
