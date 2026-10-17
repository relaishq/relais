#!/usr/bin/env bash
# Focused semantic assertions complement crash-run's real-process Redis stalls.
set -euo pipefail
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
[[ $port != 6379 ]]
work=$(mktemp -d)
pid=''
cleanup() {
  if [[ -n $pid ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT INT TERM
redis-server --bind 127.0.0.1 --port "$port" --save '' --appendonly no --dir "$work" > "$work/redis.log" 2>&1 &
pid=$!
for i in {1..100}; do
  if redis-cli -h 127.0.0.1 -p "$port" ping >/dev/null 2>&1; then break; fi
  kill -0 "$pid"
  sleep 0.05
done
export RELAIS_TEST_REDIS_ADDR="127.0.0.1:$port" RELAIS_TEST_REDIS_REQUIRE=1
go test -race -count=1 -v -timeout=5m -run '^TestV2FailedForcedWriteDoesNotDebit$' ./pkg/mediaworker
go test -race -count=1 -v -timeout=5m -run '^TestV2Tasks(ForcedCheckpointOutage|LocalResultsDuringOutage)$|^TestV2AgentTakeoverMoveOnlyUnknown$' ./pkg/callharness
