#!/usr/bin/env bash
# Cache the two real binaries, never symlinks into an uncached build tree.
set -euo pipefail
: "${VALKEY_VERSION:?}" "${VALKEY_SHA256:?}" "${RUNNER_TEMP:?}" "${GITHUB_PATH:?}"
bin="$RUNNER_TEMP/valkey-bin"
started=$SECONDS
report() {
  status=$?
  message="Valkey setup: exit $status; $((SECONDS - started))s; version $VALKEY_VERSION"
  echo "$message"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then echo "$message" >> "$GITHUB_STEP_SUMMARY"; fi
  if ((status != 0)); then echo '::error title=Valkey setup failed::Pinned Valkey download, checksum or build failed within its setup bound'; fi
}
trap report EXIT
if [[ ! -x "$bin/redis-server" || ! -x "$bin/redis-cli" ]]; then
  archive="$RUNNER_TEMP/valkey.tar.gz"
  curl --fail --location --connect-timeout 10 --max-time 45 --retry 1 --retry-max-time 100 \
    "https://codeload.github.com/valkey-io/valkey/tar.gz/refs/tags/${VALKEY_VERSION}" -o "$archive"
  echo "${VALKEY_SHA256}  $archive" > "$RUNNER_TEMP/valkey-source.sha256"
  sha256sum --check --strict "$RUNNER_TEMP/valkey-source.sha256"
  tar -xzf "$archive" -C "$RUNNER_TEMP"
  timeout --kill-after=5s 180s make -C "$RUNNER_TEMP/valkey-${VALKEY_VERSION}" -j2 BUILD_TLS=yes MALLOC=libc
  mkdir -p "$bin"
  cp "$RUNNER_TEMP/valkey-${VALKEY_VERSION}/src/valkey-server" "$bin/redis-server"
  cp "$RUNNER_TEMP/valkey-${VALKEY_VERSION}/src/valkey-cli" "$bin/redis-cli"
  (cd "$bin" && sha256sum redis-server redis-cli > SHA256SUMS)
else
  echo 'Using cached pinned Valkey binaries'
fi
(cd "$bin" && sha256sum --check --strict SHA256SUMS)
"$bin/redis-server" --version
"$bin/redis-cli" --version
echo "$bin" >> "$GITHUB_PATH"
