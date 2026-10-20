#!/usr/bin/env bash
# Install only a caller-pinned archive. Never execute unverified bytes.
set -euo pipefail
umask 077
[[ $# == 5 ]] || { echo 'usage: download.sh VERSION arm64|x86_64 SHA256 DEST COMPONENTS' >&2; exit 2; }
version=$1 arch=$2 digest=$3 dest=$4 components=$5
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([.+-][A-Za-z0-9.+-]+)?$ ]] || exit 2
[[ $digest =~ ^[a-f0-9]{64}$ ]] || exit 2
case $arch in arm64) ;; x86_64) arch=amd64 ;; *) exit 2 ;; esac
base="relais_${version}_linux_${arch}"
url="https://github.com/relaishq/relais/releases/download/v${version}/${base}.tar.gz"
# Local test seam; the stack never sets this variable.
url=${RELAIS_TEST_ARCHIVE_URL:-$url}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl --fail --silent --show-error --location --retry 3 --output "$work/archive" "$url"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work/archive")
else
  actual=$(shasum -a 256 "$work/archive")
fi
[[ ${actual%% *} == "$digest" ]] || { echo 'artifact digest mismatch; installation refused' >&2; exit 1; }
# Reject traversal, links, devices and unexpected members before extraction.
python3 - "$work/archive" "$base" "$work" <<'PY'
import sys, tarfile
archive, base, work = sys.argv[1:]
allowed = {'relais-relay', 'relais-worker', 'relais-control', 'relais-signal',
           'relais-demo', 'relais-load', 'start.sh', 'stop.sh', 'LICENSE', 'NOTICE', 'README.md'}
with tarfile.open(archive, 'r:gz') as tar:
    for member in tar.getmembers():
        name = member.name.rstrip('/')
        if name == base and member.isdir():
            continue
        if not member.isfile() or name not in {base + '/' + n for n in allowed}:
            raise SystemExit('unsafe archive; installation refused')
    tar.extractall(work)
PY
install -d -m 0755 "$dest"
for component in $components; do
  case $component in relay|worker|control|signal) ;; *) exit 2 ;; esac
  install -m 0755 "$work/$base/relais-$component" "$dest/relais-$component"
done
echo 'artifact verified and installed'
