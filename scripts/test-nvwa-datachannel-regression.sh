#!/bin/sh
# Bounded local regression gate. Owns exactly one throwaway Redis; never 6379.
set -eu
export GOPROXY=off
export GOSUMDB=off
export GOTOOLCHAIN=local
export GOFLAGS=-mod=readonly
regression_tmp=$(mktemp -d)
redis_pid=
cleanup() {
 if [ -n "$redis_pid" ]; then
  kill "$redis_pid" 2>/dev/null || true
  wait "$redis_pid" 2>/dev/null || true
 fi
}
trap cleanup EXIT HUP INT TERM
redis_port=$(python3 - <<'PY'
import socket
with socket.socket() as s:
 s.bind(('127.0.0.1',0))
 port=s.getsockname()[1]
 assert port!=6379
 print(port)
PY
)
redis-server --bind 127.0.0.1 --port "$redis_port" --save '' --appendonly no --dir "$regression_tmp" > "$regression_tmp/redis.log" 2>&1 &
redis_pid=$!
python3 - "$redis_port" <<'PY'
import socket,sys,time
for _ in range(100):
 try:
  with socket.create_connection(('127.0.0.1',int(sys.argv[1])),timeout=.1):break
 except OSError:time.sleep(.02)
else:raise SystemExit('owned Redis did not start')
PY
export RELAIS_TEST_REDIS_ADDR="127.0.0.1:$redis_port"
export RELAIS_TEST_REDIS_REQUIRE=1
export RELAIS_HARNESS_REQUIRE_FFMPEG=1
unit_started=$(date +%s)
make test-unit
printf 'WNVDC_UNIT_WALL_SECONDS=%s\n' "$(( $(date +%s) - unit_started ))"
harness_started=$(date +%s)
make test-harness-short
printf 'WNVDC_HARNESS_SHORT_WALL_SECONDS=%s\n' "$(( $(date +%s) - harness_started ))"
