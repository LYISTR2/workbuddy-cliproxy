#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
go_binary="${GO_BIN:-go}"
output="${1:-artifacts/workbuddy.so}"
mkdir -p "$(dirname "$output")"
case "$go_binary" in */*) gofmt_binary="$(dirname "$go_binary")/gofmt";; *) gofmt_binary=gofmt;; esac
test -z "$("$gofmt_binary" -l .)"
CGO_ENABLED=1 "$go_binary" vet -mod=readonly ./...
CGO_ENABLED=1 "$go_binary" test -mod=readonly -race ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
set --
if [ -n "${VERSION:-}" ]; then
  case "$VERSION" in *[!0-9A-Za-z.+-]*) echo "Invalid VERSION" >&2; exit 1;; esac
  set -- -ldflags "-X main.version=${VERSION#v}"
fi
CGO_ENABLED=1 "$go_binary" build -mod=readonly -trimpath -buildmode=c-shared "$@" -o "$output" .
