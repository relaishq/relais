#!/usr/bin/env bash
set -euo pipefail
: "${1:?usage: build.sh TASK_SCRATCH}"
nvwa_scratch=$1
mkdir -p "$nvwa_scratch/bin"
GOPROXY=off go build -mod=readonly -o "$nvwa_scratch/bin/" \
 ./cmd/relais-relay ./cmd/relais-control ./cmd/relais-worker ./cmd/relais-signal \
 ./experiments/nvwa-room/cmd/room ./experiments/nvwa-room/cmd/topology \
 ./experiments/nvwa-room/cmd/driver ./experiments/nvwa-room/cmd/backpressure
for hz in 440 880; do
 ffmpeg -v error -f lavfi -i "sine=frequency=$hz:sample_rate=48000:duration=2" \
  -c:a libopus -b:a 32k -frame_duration 20 -page_duration 20000 -y "$nvwa_scratch/tone-$hz.ogg"
done
shasum -a 256 "$nvwa_scratch"/bin/* "$nvwa_scratch"/tone-*.ogg > "$nvwa_scratch/build-sha256.txt"
