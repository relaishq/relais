# Emulated machines for the real-process driver

The Linux-only `netns` topology runs the relay, each worker, control plane,
and throwaway Redis in separate network namespaces. The caller's UDP socket
is created in a sixth namespace. A seventh namespace owns the network fabric.
Veth pairs connect the roles to two bridges inside that fabric:

- A private datacentre subnet joins the relay's worker leg, workers,
  control plane, store, and the driver (through a management veth pair).
- A separate caller subnet joins only the caller and relay's media side.
  The relay is dual-homed and does not forward IP packets between the subnets.

The fabric namespace keeps bridge forwarding independent of host firewall
rules, including Docker's host forwarding policy. The driver does not change
the host's firewall or forwarding settings.

The caller namespace permits UDP only to and from the relay's selected media
port. It has no route to the datacentre. Every trial first checks that private
HTTP and store listeners are live from the driver, then attempts those same
ports from the caller namespace and requires failure. Linux integration tests
also check that the selected UDP port works, a second live UDP port does not,
and relay public TCP is unreachable.

## Run on Linux

Use a disposable Linux environment with Go, Redis, ffmpeg, `iproute2`,
`iptables`, and `sysctl` (`procps`). Namespace creation and `setns` require
root, including `CAP_SYS_ADMIN` and `CAP_NET_ADMIN`. Build as the normal user:

```sh
make build
sudo -E env RELAIS_REDIS_ADDR= ./bin/crash-run -topology=netns -profile=lan
```

This is the binary invocation behind `make crash-run`. It keeps the existing
phase-1 thresholds and runs ten trials in each cache/PLI mode with 60 seconds
of consent observation after each takeover. Expected final output includes:

```text
PASS: 10/10 real-process SIGKILL trials mode=redis-cache+pli
PASS: 10/10 real-process SIGKILL trials mode=pli-cache-off
```

The lines also include the existing cache attribution counts. Every trial
prints `ISOLATION PASS` before starting the call. The LAN profile leaves links
unshaped. `Plan.Links` identifies each veth endpoint for later `tc netem`
profiles; other profile names are currently refused.

For a shorter development run, append `-runs=1 -after=3s`. This does not prove
the 60-second consent requirement. To exercise kernel isolation and cleanup:

```sh
go test -c -o bin/nettopology.test ./internal/nettopology
sudo -E env RELAIS_TEST_NETNS_REQUIRE=1 ./bin/nettopology.test -test.v -test.timeout=3m
```

The store binds its own private address on port 16379. Existing external
Redis addresses are refused in this topology; port 6379 remains reserved.
Redis retains protected mode and requires a random per-run password. The
driver passes it to all store and frame-cache clients through
`RELAIS_REDIS_PASSWORD`. A private temporary configuration keeps the password
out of command arguments and uploaded run logs; it is removed after Redis
startup. The driver requires an authenticated Redis PING before trials.
Private HTTP listeners receive only the explicit datacentre CIDR through
`RELAIS_PRIVATE_NETS`. Without this setting they accept loopback only;
wildcard and public listeners are always refused.

Each run has unique namespace and link names. The driver removes owned
processes, namespaces, veth pairs and bridges on completion, failure, SIGINT,
and SIGTERM, including a second interrupt during cleanup. At startup it
removes stale namespaces with its exact prefix and UID only when their owner
PID is absent. Living owner PIDs are preserved, including concurrent runs.
SIGKILL of the driver cannot run cleanup; the next run reclaims its stale
resources. Logs remain under `bin/crash-run-*`. The `netns-crash-run` CI job
runs nightly, manually, and on main pushes, and uploads logs on failure.
Existing PR jobs are unchanged.

## Run from a Mac

Run these commands **inside an existing Linux VM**, with the repository
copied or mounted into the VM. No namespace, firewall, forwarding, or bridge
settings change on macOS. The driver refuses `netns` on macOS. Installing or
starting a VM is a separate developer action.

An existing Linux container environment can also work. It needs root,
`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, permission for the `ip netns` mount operations,
and a security policy that permits `setns`. For a disposable container in a
Linux VM, a privileged container supplies these permissions. Bind-mount the
checkout, install the same Linux dependencies, and build inside the container.
Do not use the host's network namespace (`--network=host`). Container runtimes
on a Mac perform these operations inside their Linux VM, not in macOS.

## Emulation limit: signaling

The caller stays in the driver process. A locked OS thread enters its network
namespace, opens its media socket on the caller's private address, and returns
to the driver's namespace. The socket retains the caller namespace. HTTP
signaling and driver control requests use the driver's datacentre connection.
Production would need a public signaling front; this run does not implement
or prove that front. It proves media crossing the caller link, private API
isolation, and the existing caller-observed crash thresholds.
