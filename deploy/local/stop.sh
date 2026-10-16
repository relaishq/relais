#!/usr/bin/env bash
# Signal only recorded PIDs whose process start time still matches.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
state=${STATE_DIR:-$here/.relais-local}
check=0
while [[ $# -gt 0 ]]; do
  case $1 in
    -state|--state) state=$2; shift 2 ;;
    -check) check=1; shift ;;
    -h|--help) echo "usage: $0 [-state DIR] [-check]"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[[ -f $state/pids ]] || { echo 'no recorded processes'; exit 0; }
same_process() {
  local actual
  kill -0 "$pid" 2>/dev/null || return 1
  actual=$(LC_ALL=C ps -p "$pid" -o lstart=) || return 1
  [[ $actual == "$stamp" ]]
}
failed=0
uncertain=0
identity_mismatch() {
  echo "PID $pid ($name): start time changed or could not be read; not signalling it" >&2
  uncertain=$((uncertain+1))
  failed=1
}
# Keep control and Redis alive while workers drain.
for role in signal worker-1 worker-2 control relay redis; do
  while IFS='|' read -r name pid stamp; do
    [[ $name == "$role" && $pid =~ ^[0-9]+$ && -n $stamp ]] || continue
    if ! same_process; then
      if kill -0 "$pid" 2>/dev/null; then
        identity_mismatch
      fi
      continue
    fi
    if [[ $check == 1 ]]; then failed=1; continue; fi
    kill -TERM "$pid" 2>/dev/null || true
    for ((i=0; i<80; i++)); do
      same_process || break
      sleep 0.1
    done
    if same_process; then
      kill -KILL "$pid" 2>/dev/null || true
      for ((i=0; i<30; i++)); do same_process || break; sleep 0.1; done
    fi
    if same_process; then
      echo "still alive: $name $pid" >&2; failed=1
    elif kill -0 "$pid" 2>/dev/null; then
      identity_mismatch
    fi
  done < "$state/pids"
done
if [[ $uncertain -gt 0 ]]; then
  echo "shutdown not verified: $uncertain recorded PID(s) have uncertain identity" >&2
fi
[[ $failed == 0 ]] || exit 1
echo 'all recorded processes stopped'
