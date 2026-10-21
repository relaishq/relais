# Local PR checks

Run `make check` with Go, Python 3, Node.js and a TLS-capable `redis-server`
on `PATH`. The `en_US.UTF-8` locale must be available. The command installs
nothing machine-wide. Go downloads and caches the pinned lint tool as needed;
its dependencies do not change the main module's `go.mod` or `go.sum`.

The quality targets run in CI order, followed by `make test-unit` with an owned
Redis on a free loopback port. The wrapper enables the Redis and TLS test
requirements, checks the locale, disables persistence, and removes its Redis
process and temporary directory on success, failure or interruption. It never
uses port 6379. Existing Redis environment settings are replaced for this command.

CI keeps its Redis 7 service and generates the locale on `ubuntu-latest`.
Local checks use the installed Redis version and require the locale to exist;
the wrapper sets `LC_ALL=en_US.UTF-8`. macOS uses its own locale implementation
and system libraries. Passing locally does not claim an Ubuntu run.

`make check` covers the PR `quality` and `unit-tests` jobs. The call harness,
timing smoke, cluster and other CI jobs retain their separate gates.

Use `make fmt` to apply all configured formatters. `make lint` runs the pinned
lint version without installing a binary. `make lint-install` remains available
for contributors who want an installed binary.
