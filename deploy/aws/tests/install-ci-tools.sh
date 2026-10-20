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
  fetch https://archive.ubuntu.com/ubuntu/pool/main/s/systemd/systemd_255.4-1ubuntu8.17_amd64.deb systemd.deb 250345b73e42a97ee71cae7c1e470897dfad3c639eb0a7aafe4f9a3a29871cb2
  fetch https://archive.ubuntu.com/ubuntu/pool/main/s/systemd/libsystemd-shared_255.4-1ubuntu8.17_amd64.deb libsystemd.deb 8722a69d99c1a7e6f72c5389e9c6d8053d95d2f5f4fede5dc83118f735812518
  dpkg-deb --extract systemd.deb systemd
  dpkg-deb --extract libsystemd.deb systemd
else
  fetch https://download.redis.io/releases/redis-7.0.11.tar.gz redis.tar.gz ce250d1fba042c613de38a15d40889b78f7cb6d5461a27e35017ba39b07221e3
  tar -xf redis.tar.gz
  make -C redis-7.0.11 -j2 MALLOC=libc BUILD_TLS=no redis-server redis-cli
fi
