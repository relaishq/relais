#!/usr/bin/env bash
# Build only the native Linux archive needed by the container boot proof.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: build-rehearsal-archive.sh OUT' >&2; exit 2; }
[[ $(go env GOOS) == linux && $(go env GOARCH) == amd64 ]] || { echo 'requires native linux/amd64' >&2; exit 2; }
version=0.4.0
commit=$(git rev-parse HEAD)
module=$(go list -m)
base="relais_${version}_linux_amd64"
mkdir -p "$1/$base"
out=$(cd "$1" && pwd)
stage="$out/$base"
flags="-s -w -X $module/pkg/buildinfo.Version=$version -X $module/pkg/buildinfo.Commit=$commit -X $module/pkg/buildinfo.Date=rehearsal"
for binary in relais-relay relais-worker relais-control relais-signal relais-demo relais-load; do
  CGO_ENABLED=0 go build -trimpath -ldflags "$flags" -o "$stage/$binary" "./cmd/$binary"
done
CGO_ENABLED=0 go build -trimpath -ldflags "$flags" -o "$stage/relais-compat" ./scripts/release/compat
go run ./scripts/release/compat generate --version "$version" --commit "$commit" --os linux --arch amd64 --output "$stage/compatibility.json"
cp LICENSE NOTICE README.md deploy/local/start.sh deploy/local/stop.sh "$stage/"
tar -czf "$out/$base.tar.gz" -C "$out" "$base"
sha256sum "$out/$base.tar.gz"
