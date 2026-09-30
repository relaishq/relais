# Local Redis Cluster for Tests

This guide spins up a 6-node Redis Cluster locally and runs the guarded cluster integration tests.

## Prereqs
- Docker + Docker Compose
- Go toolchain

## Start cluster
```bash
docker compose -f dev/redis-cluster/docker-compose.yml up -d
# Wait ~15-30s for cluster to initialize
```

## Export addresses
Nodes are bound on localhost ports 7000-7005.
```bash
export RELAIS_TEST_REDIS_CLUSTER_ADDRS="localhost:7000,localhost:7001,localhost:7002,localhost:7003,localhost:7004,localhost:7005"
```

## Run tests
```bash
# Storage package cluster tests only
go test ./pkg/storage -run Cluster -count=1 -v

# Or full repo tests (cluster tests will be included)
go test ./... -count=1

# Makefile shortcuts
# Run only Redis Cluster tests (requires RELAIS_TEST_REDIS_CLUSTER_ADDRS)
make test-cluster

# Run Streams Consumer Group tests against single-node (non-cluster)
make test-stream-groups
```

## Stop cluster
```bash
docker compose -f dev/redis-cluster/docker-compose.yml down -v
```

## Notes
- Tests are skipped automatically if `RELAIS_TEST_REDIS_CLUSTER_ADDRS` is not set.
- Keys use hash tags `{sess:<sessionID>}` to co-locate per-session keys in the same slot.
- Streams must be enabled in code/config to exercise XREAD paths.
