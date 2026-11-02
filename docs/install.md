# Install Relais

Relais downloads contain seven programs for Linux and macOS on amd64 and arm64.
You do not need Go. You need Redis to run the browser demo or local topology.
Download the [latest release](https://github.com/relaishq/relais/releases/latest);
set `VERSION` to its tag number without the leading `v`.

## Download

Choose `linux` or `darwin` (macOS), and `amd64` (Intel/AMD) or `arm64` (Apple
silicon/ARM). For example, on an Apple silicon Mac:

```bash
VERSION=0.3.0
OS=darwin
ARCH=arm64
ARCHIVE="relais_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fLO "https://github.com/relaishq/relais/releases/download/v${VERSION}/${ARCHIVE}"
curl -fLO "https://github.com/relaishq/relais/releases/download/v${VERSION}/checksums.txt"
```

Each archive has one directory named after the archive, without `.tar.gz`.
It contains `relais-relay`, `relais-worker`, `relais-control`, `relais-signal`,
`relais-demo`, `relais-load`, `relais-compat`, `compatibility.json`, `start.sh`,
`stop.sh`, `LICENSE`, `NOTICE`, and `README.md`. `relais-compat` checks a rolling
upgrade (see [Upgrading](#upgrading)). `compatibility.json` is the release
manifest it reads. Legacy plugin runners are not included.

## Checksum

On Linux, verify the downloaded archive against the SHA-256 manifest:

```bash
sha256sum -c --ignore-missing checksums.txt
```

On macOS, select the downloaded archive's entry, then verify it:

```bash
grep "  ${ARCHIVE}\$" checksums.txt | shasum -a 256 -c -
```

The result must say `OK`. The manifest covers all four platform archives.

## Extract and quarantine

```bash
tar xzf "$ARCHIVE"
cd "${ARCHIVE%.tar.gz}"
```

On macOS, files downloaded in a browser can carry the quarantine attribute.
macOS can block these unsigned programs when you first run them. Downloads
made with `curl` are not quarantined. After checking the download source and
checksum, remove the attribute from the extracted programs and scripts if needed:

```bash
xattr -d com.apple.quarantine relais-relay relais-worker relais-control \
  relais-signal relais-demo relais-load relais-compat start.sh stop.sh
```

A message that the attribute does not exist means that file needs no change.

## Redis

Install the Redis executable:

```bash
# Debian/Ubuntu
sudo apt-get update
sudo apt-get install redis-server

# macOS (Homebrew)
brew install redis
```

On Debian/Ubuntu, installing `redis-server` normally starts a system Redis
service on port 6379. Relais does not use that service. If nothing else needs
it, you may stop it and disable automatic startup:

```bash
sudo systemctl disable --now redis-server
```

Keep the service running if another application needs it. `brew install redis`
does not start a service.

Production processes accept remote Redis on port 6379, with TLS and ACL users.
Loopback port 6379 is refused by default to protect a developer's own Redis;
`RELAIS_REDIS_ALLOW_LOCAL_6379=1` explicitly permits it. Unspecified addresses
are refused. See [Managed Redis](deploy.md#managed-redis) for connection settings.
Development and test tools ignore that opt-in and refuse 6379 on every host.
The demo starts a throwaway Redis on a free port. `start.sh` starts its own Redis
on port 16380 by default, with persistence disabled. To use another dedicated
port, pass `-redis-port 16381`. It also refuses 6379.

## Demo

```bash
./relais-demo -bin . -agent=demo
```

Open the printed URL in Chrome. Click **Start** to hear the tone agent, then
try the move, drain, and kill controls. Stop the demo with Ctrl-C; it stops its
children and Redis. If port 9101 is busy, add `-http 127.0.0.1:9311`.
The demo writes its logs into a `demo-cluster-*` directory beside the binaries.

## Local topology

Run the real process topology on this machine:

```bash
./start.sh
```

This starts Redis, the relay, two workers, control, and the HTTPS signal
frontend, in that order. Every listener binds to loopback. The default worker
agent echoes audio and video. The script prints the signal URL, CA certificate
path, token-file path, control URL, and PID-file path. Secrets are generated
once and stored in `.relais-local`, which has mode 0700. They are never printed.
Keep this directory private. The public certificate is regenerated on each
full topology start.

The default ports are Redis 16380; relay HTTP 25700 and UDP 25780/25781;
workers HTTP 25701/25702 and UDP 25711/25712; control HTTP 25710; signal HTTPS
25743. These defaults stay below Linux's default ephemeral range (32768–60999).
Set `PORT_BASE` to shift the 25700-based ports together. Use `-state DIR`
on both scripts to choose another state directory.

In another terminal, start one 60-second call through the signal frontend:

```bash
./relais-load -external-caller \
  -signaling https://127.0.0.1:25743/calls \
  -signaling-cert .relais-local/secrets/signal.pem \
  -bearer-token-file .relais-local/secrets/signal.token \
  -relay 127.0.0.1:25780 -callers 1 -duration 60s \
  -output caller.jsonl > caller.log 2> caller.stderr &
CALLER_PID=$!
```

`relais-load` waits for a request to its local start endpoint. These commands
use Python 3 to read JSON and generate a portable UTC timestamp:

```bash
# Wait until caller.log has a JSON line with type=ready, then:
CALLER_HTTP=$(python3 -c 'import json; print(next(m["http"] for m in map(json.loads, open("caller.log")) if m.get("type") == "ready"))')
AT=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=1)).isoformat())')
curl -fsS -X POST "$CALLER_HTTP/start" -H 'Content-Type: application/json' \
  -d "{\"at\":\"$AT\"}"
sleep 6
curl -fsS http://127.0.0.1:25710/status > status.json
CALL_ID=$(python3 -c 'import json; print(json.load(open("status.json"))["calls"][0]["id"])')

# Move to the other worker, then record the result in the caller report.
curl -fsS -X POST "http://127.0.0.1:25710/calls/$CALL_ID/move" > move.json
python3 -c 'import json; print(json.dumps([json.load(open("move.json"))]))' > moves.json
curl -fsS -X POST "$CALLER_HTTP/record-moves" \
  -H 'Content-Type: application/json' --data-binary @moves.json
sleep 6

# Resolve the current owner, then kill only its PID recorded by start.sh.
curl -fsS http://127.0.0.1:25710/status > status.json
OWNER=$(python3 -c 'import json; print(json.load(open("status.json"))["calls"][0]["owner"])')
OWNER_PID=$(awk -F '|' -v name="worker-${OWNER#w}" '$1==name {pid=$2} END {print pid}' .relais-local/pids)
AT=$(python3 -c 'import datetime; print(datetime.datetime.now(datetime.timezone.utc).isoformat())')
kill -9 "$OWNER_PID"
curl -fsS -X POST "$CALLER_HTTP/kill" -H 'Content-Type: application/json' \
  -d "{\"worker\":\"$OWNER\",\"at\":\"$AT\"}"
sleep 3
curl -fsS http://127.0.0.1:25710/status

# Restart the same worker with the saved binary path, ports and shared key.
./start.sh -restart-worker "$OWNER"
sleep 3
curl -fsS http://127.0.0.1:25710/status
wait "$CALLER_PID"
./stop.sh
```

After the kill, control status must show `lost_count: 0`, one entry in
`takeovers`, a new call `owner`, and `takeover_count: 1`. After restart, the
killed worker must have `dead: false`, `recovering: false`, and a fresh
`last_heartbeat`. The caller must exit 0. Its JSONL report contains one `call`
and one `done` message, plus the move and takeover observations.

`stop.sh` signals only recorded PIDs whose command names and start times still match, stops
workers while control is available, and verifies shutdown. Logs and secrets
remain for inspection and later restarts. After a verified stop, the PID file
is saved as `.relais-local/pids.stopped`. If a startup fails, see
`.relais-local/logs/`; the script stops the processes it recorded.

## Checking the build

Tag releases create a draft only after the tagged commit is on `main` and its
latest `ci.yml` push run on `main` succeeds. The release gate waits up to 90
minutes for that run. If a main CI job flakes, a maintainer can re-run the failed
CI jobs, then re-run the failed release workflow jobs. The gate reads the latest
CI attempt, including partial reruns. Manual `workflow_dispatch` runs remain
artifacts-only and skip this gate.

All seven programs accept `-version` without starting services:

```bash
./relais-worker -version
# relais-worker version=<version> commit=<short-sha> date=<UTC-build-time>
```

Non-tag archives use `0.0.0-dev+<shortsha>`. These are development builds,
not published releases.

## Upgrading

A live rollout that mixes two release versions needs a supported pair. The pair
must be listed in `scripts/release/compatibility-policy.json`. Run
`relais-compat preflight` from the new archive before each upgrade or rollback
step. It refuses an unsupported pair. The release pair test proves a listed pair
with live calls (`make release-pair`; see
[release compatibility](../scripts/release/README.md)). The
[operator procedure](../deploy/aws/README.md#compatibility-gate-before-upgrade-or-rollback)
gives the inventory and the role order. See also
[ADR 0009](adr/0009-planned-move-export-replay.md#compatibility-and-upgrade-order).

v0.3.0 supports only itself (`tested_peers: ["self"]`). Releases before v0.3.0
carry no manifest, so there is no supported pair with v0.2.0. To upgrade from
v0.2.0, stop all programs, replace them and start them again. Or start a new
pool or stack with v0.3.0.

Stop this local topology before replacing its programs. Keep the state directory
to reuse its secrets, then start with the new archive's `start.sh -state DIR`.
For a multi-host deployment, see the [deployment guide](deploy.md).

## Build from source

Go is required for this alternative:

```bash
git clone https://github.com/relaishq/relais
cd relais
make build
deploy/local/start.sh -bin bin
# Later:
deploy/local/stop.sh
```

`BIN=/absolute/path/to/bin deploy/local/start.sh` is the environment equivalent.
Source-built custom workers can use `-worker-bin PATH -agent NAME` with
`relais-demo` and `deploy/local/start.sh`. The local script also accepts
`WORKER_BIN`/`AGENT` and saves the resolved selection for worker restart. The
[agent guide](build-an-agent.md#hear-it-in-your-browser) gives the keyless example.
The download archive does not contain that example binary.

To build all four download archives, run `make dist`. Override the output or
version with `make dist DIST_VERSION=0.3.0 DIST_DIR=dist`.

## Flag reference

See [Deploy Relais](deploy.md) for the full process flag reference and deployment
limits. The scripts above are a loopback test topology for one machine.
