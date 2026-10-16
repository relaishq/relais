#!/usr/bin/env bash
set -euo pipefail
: "${1:?usage: check-stock-worker.sh OUTPUT_DIRECTORY}"
nvwa_stock_out=$1
mkdir -p "$nvwa_stock_out"
GOPROXY=off go list -mod=readonly -deps ./cmd/relais-worker > "$nvwa_stock_out/stock-worker-deps.txt"
if rg '^github.com/relaishq/relais/experiments/' "$nvwa_stock_out/stock-worker-deps.txt"; then
  echo 'FAIL: stock worker depends on experiment packages' >&2
  exit 1
fi
GOPROXY=off go build -mod=readonly -o "$nvwa_stock_out/relais-worker" ./cmd/relais-worker
go tool nm "$nvwa_stock_out/relais-worker" > "$nvwa_stock_out/stock-worker-symbols.txt"
if rg 'github.com/relaishq/relais/experiments/' "$nvwa_stock_out/stock-worker-symbols.txt"; then
  echo 'FAIL: stock worker links experiment symbols' >&2
  exit 1
fi
echo 'PASS: stock worker has no experiments/ dependencies or symbols; internal room-audio remains opt-in'
