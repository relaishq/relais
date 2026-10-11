# Private-route TCP recovery

Apply [ADR 0007](../adr/0007-private-route-tcp-recovery.md) to Linux nodes on
measured low-latency private datacentre networks. Cover relay↔workers,
control↔workers and every service↔store. Never change default, caller or
public routes. Check RTT and jitter first: a 5ms floor can produce spurious
retransmissions on congested or jittery paths.

For example, on a node whose **existing connected private route** is
`10.42.0.0/24 dev eth1 src 10.42.0.10`:

```sh
ip -4 route show exact 10.42.0.0/24
sudo ip -4 route change 10.42.0.0/24 dev eth1 src 10.42.0.10 rto_min 5ms quickack 1
ip -4 route show exact 10.42.0.0/24
```

Expected output includes `rto_min lock 5ms quickack 1`; `ip route` locks
[`rto_min` automatically](https://github.com/iproute2/iproute2/blob/v6.11.0/ip/iproute.c#L1318). For an **existing routed private network**:

```sh
ip -4 route show exact 10.43.0.0/24
sudo ip -4 route change 10.43.0.0/24 via 10.42.0.1 dev eth1 src 10.42.0.10 rto_min 5ms quickack 1
ip -4 route show exact 10.43.0.0/24
```

Replace these examples with the exact private prefix, gateway, interface,
source and routing table from the node's current configuration. Preserve its
other route attributes. Save that configuration before changing it and
restore it to roll back. Put the metrics in the deployment's persistent
network configuration; these shell commands alone do not survive reboot or
route replacement. Verify every affected node after deployment and monitor
latency and retransmission counters. Do not use a machine-wide TCP sysctl.

Crash-run applies the same metrics only to owned DC namespace routes, by
default. `-private-route-tuning=false` measures the baseline. It verifies the
observed metrics and records `private_route_tuning` in every profile report.
Every netns run also prints its state and observed routes as
`PRIVATE_ROUTE_SETTINGS`, and writes `private-route-settings.json`. The file
is retained in CI even for the unshaped LAN crash job. Untuned runs read back
the existing routes without changing them; absent explicit metrics remain
unspecified.
Host, caller and fabric routes stay unchanged; namespace teardown removes the
settings. The driver-owned HTTP fault proxy still uses the host's original
TCP policy, so this harness does not promise to eliminate every loss spike.
