#!/usr/bin/env bash
# Collect one side of a release pair for `crash-run -release-pair`.
#   fetch <version> <out-dir>            download published release v<version>
#   build <version> <source> <out-dir>   build with <source>'s own build-archives.sh;
#                                        <source> is a clean checkout directory or
#                                        a commit, tag or origin/<branch> in this repository
# The output directory must be new or empty. Digests are checked by crash-run.
set -euo pipefail
usage() { echo "usage: $0 fetch <version> <out-dir> | build <version> <source> <out-dir>" >&2; exit 2; }
[[ $# -ge 3 ]] || usage
mode=$1
version=${2#v}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]] || { echo 'invalid release version' >&2; exit 2; }
case $mode in
  fetch) [[ $# == 3 ]] || usage; out=$3 ;;
  build) [[ $# == 4 ]] || usage; source=$3; out=$4 ;;
  *) usage ;;
esac
if [[ -e $out ]] && [[ -n $(ls -A "$out") ]]; then
  echo "output directory is not empty: $out" >&2
  exit 2
fi
mkdir -p "$out"
out=$(cd "$out" && pwd)
root=$(cd "$(dirname "$0")/../.." && pwd)
if [[ $mode == fetch ]]; then
  os=$(go env GOOS)
  arch=$(go env GOARCH)
  # Only the host archive and the release's own checksum list are needed.
  gh release download "v$version" --dir "$out" \
    --pattern "relais_${version}_${os}_${arch}.tar.gz" --pattern checksums.txt
  ls "$out"
  exit 0
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/relais-pair-src.XXXXXX")
trap 'rm -rf "$work"' EXIT
if [[ -d $source ]]; then
  checkout=$(cd "$source" && pwd)
  git -C "$checkout" rev-parse --verify HEAD >/dev/null
  # The stamped commit must identify every built byte.
  if [[ -n $(git -C "$checkout" status --porcelain --untracked-files=normal) ]]; then
    echo "checkout has uncommitted changes; commit them so the stamped commit identifies the build: $checkout" >&2
    exit 2
  fi
else
  commit=$(git -C "$root" rev-parse --verify --end-of-options "$source^{commit}")
  # A shared clone reads this repository's objects without adding a worktree to it.
  git clone --quiet --shared --no-checkout "$root" "$work/src"
  git -C "$work/src" checkout --quiet --detach "$commit"
  checkout=$work/src
fi
"$checkout/scripts/release/build-archives.sh" "$version" "$out"
