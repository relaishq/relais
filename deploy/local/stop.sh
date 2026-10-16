#!/usr/bin/env bash
# Signal only recorded PIDs whose command and process start time still match.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
state=${STATE_DIR:-$here/.relais-local}
check=0
only=''
while [[ $# -gt 0 ]]; do
  case $1 in
    -state|--state) state=$2; shift 2 ;;
    -check) check=1; shift ;;
    -check-worker) check=1; only=$2; shift 2 ;;
    -h|--help) echo "usage: $0 [-state DIR] [-check] [-check-worker worker-1|worker-2]"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[[ -z $only || $only == worker-1 || $only == worker-2 ]] || { echo 'invalid worker name' >&2; exit 2; }
[[ -f $state/pids ]] || { echo 'no recorded processes'; exit 0; }
# 0: same process; 1: gone or PID reused by a different command; 2: uncertain.
process_state() {
  local actual actual_comm
  kill -0 "$pid" 2>/dev/null || return 1
  [[ -n $comm ]] || return 2 # Old-format live records cannot establish identity.
  if ! actual_comm=$(LC_ALL=C ps -p "$pid" -o comm=); then
    if kill -0 "$pid" 2>/dev/null; then return 2; else return 1; fi
  fi
  actual_comm=${actual_comm#"${actual_comm%%[![:space:]]*}"}
  actual_comm=${actual_comm%"${actual_comm##*[![:space:]]}"}
  actual_comm=${actual_comm##*/}
  # Keep only the executable name: macOS ps reports a rewritten process
  # title (redis-server shows "redis-server 127.0.0.1:PORT").
  actual_comm=${actual_comm%% *}
  [[ -n $actual_comm ]] || return 2
  [[ $actual_comm == "$comm" ]] || return 1
  if ! actual=$(LC_ALL=C ps -p "$pid" -o lstart=); then
    if kill -0 "$pid" 2>/dev/null; then return 2; else return 1; fi
  fi
  [[ $actual == "$stamp" ]] || return 2
}
same_process() { process_state; }
failed=0
uncertain=0
identity_mismatch() {
  echo "PID $pid ($name): same or unknown command with changed/unreadable start time; not signalling it" >&2
  uncertain=$((uncertain+1))
  failed=1
}
# Keep control and Redis alive while workers drain.
for role in signal worker-1 worker-2 control relay redis; do
  [[ -z $only || $role == "$only" ]] || continue
  while IFS='|' read -r name pid stamp comm; do
    [[ $name == "$role" && $pid =~ ^[0-9]+$ && -n $stamp ]] || continue
    if process_state; then status=0; else status=$?; fi
    case $status in
      1) continue ;;
      2) identity_mismatch; continue ;;
    esac
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
    if process_state; then status=0; else status=$?; fi
    case $status in
      0) echo "still alive: $name $pid" >&2; failed=1 ;;
      2) identity_mismatch ;;
    esac
  done < "$state/pids"
done
if [[ $uncertain -gt 0 ]]; then
  echo "shutdown not verified: $uncertain recorded PID(s) have uncertain identity" >&2
  echo 'Recovery: inspect each PID above with ps -p PID -o pid=,lstart=,comm=; confirm no recorded Relais/Redis process still belongs to this topology.' >&2
  printf 'Only after that check, remove the stale record file: rm -- %q\n' "$state/pids" >&2
fi
[[ $failed == 0 ]] || exit 1
if [[ -z $only ]]; then mv "$state/pids" "$state/pids.stopped"; fi
echo 'all recorded processes stopped'
