#!/usr/bin/env bash
# Build the complete download contract, with no host-specific dependencies.
set -euo pipefail
[[ $# == 2 ]] || { echo "usage: $0 <version> <out-dir>" >&2; exit 2; }
root=$(cd "$(dirname "$0")/../.." && pwd)
version=${1#v}
[[ $version =~ ^[0-9][A-Za-z0-9.+-]*$ ]] || { echo 'invalid archive version' >&2; exit 2; }
mkdir -p "$2"
out=$(cd "$2" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/relais-dist.XXXXXX")
trap 'rm -rf "$work"' EXIT
cd "$root"
commit=$(git rev-parse HEAD)
built=$(date -u '+%Y-%m-%d_%H:%M:%S')
module=$(awk '$1 == "module" {print $2; exit}' go.mod)
# -s -w drop the symbol table and DWARF; panics still print function names.
ldflags="-s -w -X $module/pkg/buildinfo.Version=$version -X $module/pkg/buildinfo.Commit=$commit -X $module/pkg/buildinfo.Date=$built"
go run ./scripts/release/compat check
binaries=(relais-relay relais-worker relais-control relais-signal relais-demo relais-load)
archives=()
for os in linux darwin; do
  for arch in amd64 arm64; do
    base="relais_${version}_${os}_${arch}"
    stage="$work/$base"
    mkdir -p "$stage"
    for binary in "${binaries[@]}"; do
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$stage/$binary" "./cmd/$binary"
    done
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$stage/relais-compat" ./scripts/release/compat
    go run ./scripts/release/compat generate --version "$version" --commit "$commit" --os "$os" --arch "$arch" --output "$stage/compatibility.json"
    cp LICENSE NOTICE README.md "$stage/"
    cp deploy/local/start.sh deploy/local/stop.sh "$stage/"
    chmod 755 "$stage/start.sh" "$stage/stop.sh"
    # BSD tar otherwise adds AppleDouble metadata outside the top-level directory.
    COPYFILE_DISABLE=1 tar -czf "$out/$base.tar.gz" -C "$work" "$base"
    archives+=("$base.tar.gz")
  done
done
cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "${archives[@]}" > checksums.txt
else
  shasum -a 256 "${archives[@]}" > checksums.txt
fi
cd "$root"
python3 scripts/release/check-archives.py "$out" "$version" "$commit"
printf '%s\n' "${archives[@]}" checksums.txt
