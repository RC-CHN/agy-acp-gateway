#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local GOPROXY=off
go vet ./agy_acp_gateway
env -u AGY_REAL_BINARY -u AGY_GATEWAY_BINARY go test -race -count=1 ./agy_acp_gateway
if [[ ${1:-} == --real ]]; then
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  CGO_ENABLED=0 go test -c -o "$work/verify" ./agy_acp_gateway
  CGO_ENABLED=0 go build -buildvcs=false -o "$work/gateway" ./agy_acp_gateway
  export AGY_GATEWAY_BINARY="$work/gateway"
  if [[ -z ${AGY_REAL_BINARY:-} ]]; then
    if command -v agy >/dev/null 2>&1; then
      AGY_REAL_BINARY=$(command -v agy)
      export AGY_REAL_BINARY
    else
      echo "error: set AGY_REAL_BINARY to your local agy executable" >&2
      exit 1
    fi
  fi
  if awk -F: 'NF == 2 && $1 !~ /^[[:space:]]*lo$/ {found=1} END {exit found}' /proc/net/dev; then
    "$work/verify" -test.v -test.run '^TestReal' -test.timeout 5m
  else
    unshare --user --map-root-user --net sh -ec 'ip link set lo up; exec "$1" -test.v -test.run "^TestReal" -test.timeout 5m' sh "$work/verify"
  fi
fi
