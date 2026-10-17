#!/usr/bin/env bash
set -euo pipefail
: "${1:?usage: build.sh TASK_SCRATCH}"
nvwa_scratch=$1
mkdir -p "$nvwa_scratch/bin"
GOPROXY=off go build -mod=readonly -o "$nvwa_scratch/bin/" \
 ./cmd/relais-relay ./cmd/relais-control ./cmd/relais-worker ./cmd/relais-signal \
 ./experiments/nvwa-room/cmd/room ./experiments/nvwa-room/cmd/topology \
 ./experiments/nvwa-room/cmd/network-driver ./experiments/nvwa-room/cmd/driver ./experiments/nvwa-room/cmd/burst ./experiments/nvwa-room/cmd/backpressure ./experiments/nvwa-room/cmd/policies
for hz in 440 880; do
 ffmpeg -v error -f lavfi -i "sine=frequency=$hz:sample_rate=48000:duration=2" \
  -c:a libopus -b:a 32k -frame_duration 20 -page_duration 20000 -y "$nvwa_scratch/tone-$hz.ogg"
done
shasum -a 256 "$nvwa_scratch"/bin/* "$nvwa_scratch"/tone-*.ogg > "$nvwa_scratch/build-sha256.txt"

python3 - "$nvwa_scratch" <<'PYTHON'
import hashlib,json,pathlib,sys
roots = ['cmd','internal','pkg','experiments/nvwa-room']
paths = [p for root in roots for p in pathlib.Path(root).rglob('*.go')]
paths += [pathlib.Path('go.mod'),pathlib.Path('go.sum')]
pathlib.Path(sys.argv[1], 'runtime-source-sha256.json').write_text(json.dumps({str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(paths)},indent=2)+'\n')
PYTHON
