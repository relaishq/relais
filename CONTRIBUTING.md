# Contributing to Relais

Build a focused change, keep error handling and context cancellation explicit,
and run the checks that exercise it. Concurrent code must be race-free. Add
integration coverage for new legacy plugins. Open a pull request from your
feature branch after testing and linting. Contributors can fork and clone
[relaishq/relais](https://github.com/relaishq/relais); source builds need Go 1.26+.
See [CLAUDE.md](CLAUDE.md) for the repository's command and CI contract.

## Build and local tools

```sh
make deps
make build
make fmt
make vet
make lint
```

`make lint-install` installs the pinned golangci-lint version used by CI.
Use `make clean` to remove build artifacts. `make demo-agent` runs the scripted
agent, `make demo-cluster` runs the echo cluster, and `make demo` runs the older
shared-socket page. See [demo instructions](docs/demo.md).

## Redis and tests

Never connect tests or tools to Redis on port **6379**. Use a dedicated,
throwaway instance. The process binaries refuse 6379; legacy defaults are not
permission to use it. For example, start this Redis in a separate terminal,
wait for its ready log, and stop it with Ctrl-C after your tests:

```sh
redis_dir="$(mktemp -d)"
redis-server --bind 127.0.0.1 --port 16379 --save '' --appendonly no --dir "$redis_dir"
```

Then, from the repository root:

```sh
export RELAIS_TEST_REDIS_ADDR=127.0.0.1:16379
export RELAIS_TEST_REDIS_REQUIRE=1
export RELAIS_HARNESS_REQUIRE_FFMPEG=1
make test-unit
make test-harness-short
go test -race -v -count=1 -timeout 5m \
  -run '^TestRelayUncertainTransferSurvives$' ./pkg/callharness/
make test-harness-smoke
```

Install `ffmpeg` on PATH for full VP8 decode in the harness. `make test-unit`
runs every non-harness package under the race detector with a ten-minute limit.
The short harness uses memory only and five-second baseline calls; longer fault
scenarios keep their fixed durations. It skips the long planned-consent case
and Redis uncertainty cases. The separate Redis race command above restores
uncertainty coverage. The timing smoke runs without the race detector and keeps
full consent windows, planned moves, hard kills, and planned sequence wrap.
Both short targets clear the Redis test address and disable its requirement.

Additional targets:

```sh
make test              # all tests, not the CI partition; can take longer
make test-harness      # full race harness
make test-harness-timing
make test-full         # race units and both full harness lanes
make bench             # memory always; Redis when explicitly configured
make profile           # benchmark CPU and memory profiles
make coverage          # coverage.out and HTML report
```

Without `RELAIS_TEST_REDIS_ADDR`, Redis tests and benchmarks skip before
connecting. With `RELAIS_TEST_REDIS_REQUIRE=1`, missing or unreachable Redis
fails those checks. Redis benchmarks use random prefixes and remove their keys.
See [legacy stream tests](docs/legacy/plugin-pipeline.md#running-redis-streams-integration-tests)
for `make test-streams`, `make test-stream-groups`, and optional `make test-cluster`.

## PR and full checks

PR CI runs lint, build, vet, the Node demo-page tests, race units with Redis,
the short memory harness, the Redis uncertainty race case, and the memory timing
smoke. The smoke keeps the full planned-move and hard-kill counts and consent
checks. Jobs run in parallel; the `build` job aggregates their results. Making
it required in GitHub settings is a separate admin decision. The target wall
time is about ten minutes, not a measured promise. Superseded PR runs cancel;
full-matrix runs do not cancel under that policy.

Pushes to `main`, nightly runs at 08:00 UTC, and manual workflow dispatch add
the full race harness, timing gates on both stores, benchmarks, and real-process
crash trials. The optional Redis Cluster gate remains separate. Use
**Actions → CI → Run workflow** for manual runs. Nightly/manual store-stall
checks and their thresholds are described in [CLAUDE.md](CLAUDE.md) and
[store outage guarantees](docs/guarantees.md#store-outages).

## Real process crashes

`crash-run` starts a relay, control, workers, and its own disposable Redis on a
free non-6379 port. It needs `redis-server` and `ffmpeg` on PATH:

```sh
make crash-run
make crash-run CRASH_FLAGS="-runs=1 -after=3s -verbose"
make crash-run CRASH_FLAGS="-runs=1 -after=3s -sigterm -verbose"
```

The default is the full acceptance run. `-after` below the full consent window
marks that acceptance check `NOT-RUN`; it is only a developer smoke. `-sigterm`
checks graceful draining instead of crash takeover. Logs remain under
`bin/crash-run-*`, and failed CI runs retain them as artifacts. See
[deployment and crash-run details](docs/deploy.md#real-processes-webrtc-you-can-kill--9)
for cleanup, private APIs, and measurement meaning.

## Design and evidence

- [Developer guides](docs/dev/) describe tests, capacity, networks, and stores.
- [Architecture decisions](docs/adr/) define accepted protocol and safety rules.
- [Agent contributor guides](docs/agents/) cover issues, triage, and domain docs.
- [Guarantees](docs/guarantees.md) separate observed results from non-guarantees.
- [Architecture](docs/architecture.md) describes the current media path.
