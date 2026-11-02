#!/usr/bin/env bash
# One-machine topology. All listeners are loopback; Redis is disposable.
set -euo pipefail
umask 077
here=$(cd "$(dirname "$0")" && pwd)
bin=${BIN:-$here}
state=${STATE_DIR:-$here/.relais-local}
redis_port=${REDIS_PORT:-16380}
port_base=${PORT_BASE:-25700}
agent=${AGENT:-}
worker_bin=${WORKER_BIN:-}
restart=''
while [[ $# -gt 0 ]]; do
  case $1 in
    -bin|--bin) bin=$2; shift 2 ;;
    -worker-bin|--worker-bin) worker_bin=$2; shift 2 ;;
    -agent|--agent) agent=$2; shift 2 ;;
    -state|--state) state=$2; shift 2 ;;
    -redis-port|--redis-port) redis_port=$2; shift 2 ;;
    -restart-worker|--restart-worker) restart=$2; shift 2 ;;
    -h|--help) echo "usage: $0 [-bin DIR] [-worker-bin PATH -agent NAME] [-agent NAME] [-state DIR] [-redis-port PORT] [-restart-worker w1|w2]"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
if [[ -z $restart && -n $worker_bin && -z ${agent//[[:space:]]/} ]]; then
  echo '-agent is required with -worker-bin (or set AGENT with WORKER_BIN)' >&2; exit 2
fi
mkdir -p "$state"
chmod 700 "$state"
state=$(cd "$state" && pwd)
mkdir "$state/start.lock" 2>/dev/null || { echo "start already in progress: $state/start.lock" >&2; exit 1; }
started=0
finish() {
  code=$?
  trap - EXIT
  if [[ $code != 0 && $started == 1 ]]; then "$here/stop.sh" -state "$state" >&2 || true; fi
  rmdir "$state/start.lock"
  exit "$code"
}
trap finish EXIT
if [[ -n $restart ]]; then
  [[ $restart == w1 || $restart == w2 ]] || { echo 'restart requires w1 or w2' >&2; exit 2; }
  [[ -f $state/config ]] || { echo 'start the topology first' >&2; exit 1; }
  # Written by this script inside the owner's private state directory.
  # shellcheck source=/dev/null
  source "$state/config"
else
  if [[ -f $state/pids ]]; then
    "$here/stop.sh" -state "$state" -check || { echo 'topology already running; stop it first' >&2; exit 1; }
  fi
  bin=$(cd "$bin" && pwd)
fi
[[ -z $worker_bin || -n ${agent//[[:space:]]/} ]] || { echo '-agent is required with -worker-bin' >&2; exit 2; }
agent=${agent:-echo}
[[ $redis_port =~ ^[0-9]+$ && ${#redis_port} -le 5 ]] || { echo 'invalid Redis port' >&2; exit 2; }
redis_port=$((10#$redis_port))
[[ $redis_port -gt 0 && $redis_port -le 65535 && $redis_port != 6379 ]] || { echo 'Redis port 6379 is reserved; choose another port in 1..65535' >&2; exit 2; }
[[ $port_base =~ ^[0-9]+$ && ${#port_base} -le 5 ]] || { echo 'invalid PORT_BASE' >&2; exit 2; }
port_base=$((10#$port_base))
[[ $port_base -gt 0 && $port_base -le 65454 ]] || { echo 'PORT_BASE must be in 1..65454' >&2; exit 2; }
[[ -n $worker_bin || $agent == echo || $agent == demo ]] || { echo 'AGENT must be echo or demo unless -worker-bin is set' >&2; exit 2; }
worker_bin=${worker_bin:-$bin/relais-worker}
[[ -x $worker_bin ]] || { echo "missing worker binary: $worker_bin" >&2; exit 1; }
# Persist an absolute path so restart does not depend on its working directory.
# Shell expansion only: the install test runs with a minimal tool PATH.
worker_dir=.
[[ $worker_bin == */* ]] && worker_dir=${worker_bin%/*}
worker_bin=$(cd "${worker_dir:-/}" && pwd)/${worker_bin##*/}
for name in relais-relay relais-control relais-signal; do
  [[ -x $bin/$name ]] || { echo "missing binary: $bin/$name (use -bin bin in a source checkout)" >&2; exit 1; }
done
for tool in redis-server openssl curl ps nohup; do command -v "$tool" >/dev/null || { echo "missing dependency: $tool" >&2; exit 1; }; done
mkdir -p "$state/logs" "$state/secrets" "$state/redis"
[[ -f $state/secrets/sessionstore.key ]] || openssl rand -hex 32 > "$state/secrets/sessionstore.key"
[[ -f $state/secrets/signal.token ]] || openssl rand -hex 32 > "$state/secrets/signal.token"
# Never inherit a user's Redis credentials, prefix, or non-loopback settings.
for redis_var in "${!RELAIS_REDIS_@}"; do unset "$redis_var"; done
unset RELAIS_SESSIONSTORE_KEY RELAIS_SESSIONSTORE_KEY_ID RELAIS_SESSIONSTORE_OLD_KEYS RELAIS_PRIVATE_NETS RELAIS_SIGNAL_TOKEN
export RELAIS_REDIS_ADDR="127.0.0.1:$redis_port"
export RELAIS_REDIS_PREFIX="relais:local:$redis_port:"
ctrl="http://127.0.0.1:$((port_base+10))"
start() {
  local name=$1 pid stamp comm identity_recorded=0 i
  shift
  if [[ -f $state/logs/$name.log ]]; then mv "$state/logs/$name.log" "$state/logs/$name.previous.log"; fi
  nohup "$@" > "$state/logs/$name.log" 2>&1 < /dev/null &
  pid=$!
  # nohup may not have exec'd the program when the first ps runs.
  for ((i=0; i<100; i++)); do
    if ! stamp=$(LC_ALL=C ps -p "$pid" -o lstart=) || ! comm=$(LC_ALL=C ps -p "$pid" -o comm=); then break; fi
    comm=${comm#"${comm%%[![:space:]]*}"}
    comm=${comm%"${comm##*[![:space:]]}"}
    comm=${comm##*/}
    # Keep only the executable name: macOS ps reports a rewritten process
    # title (redis-server shows "redis-server 127.0.0.1:PORT").
    comm=${comm%% *}
    if [[ $comm == "${1##*/}" ]]; then identity_recorded=1; break; fi
    sleep 0.01
  done
  if [[ $identity_recorded != 1 ]]; then
    echo "$name identity could not be recorded; stopping our child $pid" >&2
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    return 1
  fi
  printf '%s|%s|%s|%s\n' "$name" "$pid" "$stamp" "$comm" >> "$state/pids"
}
wait_ready() {
  local name=$1 pattern=$2 pid i
  pid=$(awk -F '|' -v name="$name" '$1==name {pid=$2} END {print pid}' "$state/pids")
  for ((i=0; i<150; i++)); do
    kill -0 "$pid" 2>/dev/null || { echo "$name exited; see $state/logs/$name.log" >&2; return 1; }
    if grep -q "$pattern" "$state/logs/$name.log"; then return 0; fi
    sleep 0.1
  done
  echo "$name readiness timed out; see $state/logs/$name.log" >&2
  return 1
}
worker() {
  local n=$1
  RELAIS_SESSIONSTORE_KEY=$(cat "$state/secrets/sessionstore.key")
  export RELAIS_SESSIONSTORE_KEY
  start "worker-$n" "$worker_bin" -name "w$n" -media "127.0.0.1:$((port_base+10+n))" -http "127.0.0.1:$((port_base+n))" \
    -relay-leg "127.0.0.1:$((port_base+81))" -relay-media "127.0.0.1:$((port_base+80))" -control "$ctrl" -agent "$agent"
  unset RELAIS_SESSIONSTORE_KEY RELAIS_SESSIONSTORE_KEY_ID RELAIS_SESSIONSTORE_OLD_KEYS
  wait_ready "worker-$n" '"pid"'
}
if [[ -n $restart ]]; then
  name="worker-${restart#w}"
  "$here/stop.sh" -state "$state" -check-worker "$name" || {
    echo "$name is still running or has uncertain identity; refusing duplicate start" >&2; exit 1;
  }
  # Replace this worker's old record, preserving the other topology processes.
  awk -F '|' -v name="$name" '$1 != name' "$state/pids" > "$state/pids.next"
  mv "$state/pids.next" "$state/pids"
  worker "${restart#w}"
  echo "$restart restarted; PID recorded in $state/pids"
  exit 0
fi
: > "$state/pids"
printf 'bin=%q\nredis_port=%q\nport_base=%q\nagent=%q\nworker_bin=%q\n' "$bin" "$redis_port" "$port_base" "$agent" "$worker_bin" > "$state/config"
started=1
start redis redis-server --bind 127.0.0.1 --port "$redis_port" --save '' --appendonly no --dir "$state/redis"
wait_ready redis 'Ready to accept connections'
unset RELAIS_SESSIONSTORE_KEY RELAIS_SESSIONSTORE_KEY_ID RELAIS_SESSIONSTORE_OLD_KEYS
start relay "$bin/relais-relay" -media "127.0.0.1:$((port_base+80))" -leg "127.0.0.1:$((port_base+81))" -http "127.0.0.1:$port_base"
wait_ready relay '"pid"'
worker 1
worker 2
RELAIS_SESSIONSTORE_KEY=$(cat "$state/secrets/sessionstore.key")
export RELAIS_SESSIONSTORE_KEY
start control "$bin/relais-control" \
  -http "127.0.0.1:$((port_base+10))" -relay "http://127.0.0.1:$port_base" \
  -worker "w1=http://127.0.0.1:$((port_base+1))" -worker "w2=http://127.0.0.1:$((port_base+2))"
unset RELAIS_SESSIONSTORE_KEY RELAIS_SESSIONSTORE_KEY_ID RELAIS_SESSIONSTORE_OLD_KEYS
wait_ready control '"pid"'
if [[ -f $state/secrets/signal.pem ]]; then mv "$state/secrets/signal.pem" "$state/secrets/signal.previous.pem"; fi
start signal "$bin/relais-signal" -listen "127.0.0.1:$((port_base+43))" -control "$ctrl" \
  -advertise-ips 127.0.0.1 -token-file "$state/secrets/signal.token" -cert-out "$state/secrets/signal.pem"
wait_ready signal 'https='
printf 'Signal URL: https://127.0.0.1:%s\nCA certificate: %s\nToken file: %s\nControl URL: %s\nPID file: %s\n' \
  "$((port_base+43))" "$state/secrets/signal.pem" "$state/secrets/signal.token" "$ctrl" "$state/pids"
