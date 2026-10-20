#!/usr/bin/env bash
# Only for disposable GitHub-hosted Ubuntu runners. Never installs a Redis service.
set -euo pipefail
[[ $# == 2 && ( $1 == lint || $1 == boot ) ]] || exit 2
mode=$1 dest=$2
mkdir -p "$dest"
cd "$dest"
fetch() {
  local url=$1 filename=$2 sha=$3
  curl --fail --silent --show-error --location --output "$filename" "$url"
  printf '%s  %s\n' "$sha" "$filename" | sha256sum --check --strict
}
if [[ $mode == lint ]]; then
  fetch https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.x86_64.tar.xz shellcheck.tar.xz 8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
  tar -xf shellcheck.tar.xz

else
  fetch https://download.redis.io/releases/redis-7.0.11.tar.gz redis.tar.gz ce250d1fba042c613de38a15d40889b78f7cb6d5461a27e35017ba39b07221e3
  tar -xf redis.tar.gz
  # Serialize recursive dependency builds too; parallel Lua compilation can omit luaopen_bit.
  make -C redis-7.0.11 -j1 MALLOC=libc BUILD_TLS=no redis-server redis-cli
fi
