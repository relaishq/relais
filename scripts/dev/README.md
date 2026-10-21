# Local PR checks

Run `make check` with Go, Python 3, Node.js and a TLS-capable `redis-server`
on `PATH`. The `en_US.UTF-8` locale must be available. The command installs
nothing machine-wide. Go downloads and caches the pinned lint tool as needed;
its dependencies do not change the main module's `go.mod` or `go.sum`.

The quality targets run in CI order, followed by `make test-unit` with an owned
Redis on a free loopback port. The wrapper enables the Redis and TLS test
requirements, checks the locale, disables persistence, and removes its Redis
process and temporary directory on success, failure or interruption. It never
uses port 6379. Before starting Redis or the command, it removes every inherited
`RELAIS_TEST_*`, `RELAIS_REDIS_*` and `RELAIS_UPDATE_*` variable. It then sets only
`RELAIS_TEST_REDIS_ADDR` to its owned endpoint, `RELAIS_TEST_REDIS_REQUIRE=1`,
`RELAIS_TEST_REDIS_TLS_REQUIRE=1`, and `LC_ALL=en_US.UTF-8`. Other environment
variables are preserved. This removes inherited cluster, auth and golden-update
opt-ins. On exit it terminates the command's owned process group, including
background descendants. It waits up to five seconds for the command to exit,
then sends SIGKILL to the owned group to stop any remaining descendants.

CI keeps its Redis 7 service and generates the locale on `ubuntu-latest`.
Local checks use the installed Redis version and require the locale to exist;
the wrapper sets `LC_ALL=en_US.UTF-8`. macOS uses its own locale implementation
and system libraries. Passing locally does not claim an Ubuntu run.

`make check` covers the PR `quality` and `unit-tests` jobs. The call harness,
timing smoke, cluster and other CI jobs retain their separate gates.

Use `make fmt` to apply all configured formatters. `make lint` runs the pinned
lint version without installing a binary. `make lint-install` remains available
for contributors who want an installed binary.

The cleanup tests in `scripts/ci/test_with_redis.py` use a loopback PING stub and
inject a locale check into the wrapper function. These tests require neither a
real Redis nor a generated locale, so they run in the CI quality job. The CLI
always uses the real locale check. A real-Redis case runs as an additional test
when the executable and locale are available.
