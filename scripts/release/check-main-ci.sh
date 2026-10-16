#!/usr/bin/env bash
# Require the tagged commit's latest main push CI attempt to pass.
# Exit: 0 success; 1 unsuccessful CI; 2 missing run/ancestry or lookup error;
# 3 timeout. --wait-minutes 0 checks once without waiting.
set -euo pipefail

usage() {
  echo "usage: $0 <sha> [--wait-minutes N (0-90)]" >&2
  exit 2
}
[[ $# == 1 || $# == 3 ]] || usage
[[ $1 =~ ^[0-9a-fA-F]{7,40}$ ]] || usage
wait_minutes=90
if [[ $# == 3 ]]; then
  [[ $2 == --wait-minutes && $3 =~ ^[0-9]{1,2}$ ]] || usage
  wait_minutes=$((10#$3))
  (( wait_minutes <= 90 )) || usage
fi

# Fetch complete ancestry, including when invoked from a shallow local clone.
fetch_args=(--no-tags)
if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
  fetch_args+=(--unshallow)
fi
if ! git fetch "${fetch_args[@]}" origin '+refs/heads/main:refs/remotes/origin/main'; then
  echo 'Cannot fetch origin/main to verify ancestry.' >&2
  exit 2
fi
if ! sha=$(git rev-parse --verify "$1^{commit}"); then
  echo "Commit not found: $1" >&2
  exit 2
fi
if ! git merge-base --is-ancestor "$sha" origin/main; then
  echo "Commit $sha is not an ancestor of origin/main." >&2
  exit 2
fi

deadline=$((SECONDS + wait_minutes * 60))
while :; do
  # gh lists newest runs first. Query again on every poll so reruns use the
  # current attempt and conclusion, including a rerun of only failed jobs.
  if ! run=$(gh run list --workflow ci.yml --branch main --event push \
    --commit "$sha" --limit 1 \
    --json databaseId,status,conclusion,attempt,url --jq '.[0] // empty'); then
    echo "Cannot query main push CI for $sha." >&2
    exit 2
  fi
  if [[ -z $run ]]; then
    echo "No ci.yml push run on main for $sha." >&2
    exit 2
  fi
  IFS=$'\t' read -r run_id status conclusion attempt url < <(
    jq -r '[.databaseId, .status, (.conclusion // "" | if . == "" then "pending" else . end), .attempt, .url] | @tsv' <<< "$run"
  )
  printf '%s main CI: status=%s conclusion=%s attempt=%s %s\n' \
    "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$status" "$conclusion" "$attempt" "$url"
  if [[ $status == completed ]]; then
    if [[ $conclusion == success ]]; then
      exit 0
    fi
    echo "Main CI did not succeed: $url" >&2
    echo 'Failed/cancelled/skipped jobs:' >&2
    # Do not pin an old attempt: the default includes each job's latest result.
    if ! gh run view "$run_id" --json jobs --jq \
      '.jobs[] | select(.conclusion != "success" and .conclusion != "" and .conclusion != null) | "  \(.name): \(.conclusion)"' >&2; then
      echo 'Could not retrieve failed job names.' >&2
    fi
    exit 1
  fi
  remaining=$((deadline - SECONDS))
  if (( remaining <= 0 )); then
    echo "Timed out after $wait_minutes minute(s) waiting for main CI: $url" >&2
    exit 3
  fi
  delay=60
  if (( remaining < delay )); then delay=$remaining; fi
  sleep "$delay"
done
