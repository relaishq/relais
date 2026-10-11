# ADR 0004: Fence a same-host standby relay before binding

A relay restart loses every call's media until its sockets and routes recover.
The standalone `relais-relay` now owns a fenced relay lease. A second instance
with `-standby` waits without binding sockets and takes over the same public UDP,
worker-leg UDP and private HTTP addresses after lease expiry. Both processes use
the same Redis namespace and `-relay-lease` key (default `default`). Each socket
set needs a distinct key. Standby addresses must have fixed, nonzero ports.
Neither UDP socket uses `SO_REUSEPORT`; kernel socket exclusivity remains a
second defence against simultaneous forwarding.

## Lease and fencing order

Memory and Redis implement an independent `RelayLeases` capability with claim,
renew, transfer, activation and read. The owner is a fresh instance token, with a
monotone epoch, expiry, PID and kernel start time. The relay's keyless
`RedisOwners` capability gains these metadata operations, not snapshot access.
Redis uses one atomic Lua script and Redis `TIME`; memory uses its lease lock.
The Redis hash is persistent: expiry must not erase fencing evidence or reset an
epoch. Keep namespaces and keys stable while old processes can exist. Do not
share a relay key across hosts or remove it while its processes can exist.

Takeover follows this order:

1. Claim the expired lease, advancing its epoch. An uncertain store result is
   never permission to signal or bind. Repeating a claim for the identical
   process identity settles it idempotently if that tenure still exists.
2. Start renewing the new tenure, including during fencing and route restore.
3. Fence the old lease holder and the last possible forwarder, deduplicating
   identical targets. Check PID and kernel start time immediately before
   `SIGKILL`, then wait for exit. A zombie is already unable to forward or hold
   sockets. Linux reads `/proc/<pid>/stat`; macOS reads `kern.proc.pid` through
   `golang.org/x/sys/unix`. A missing process is already fenced. A start-time
   mismatch, permission failure or unconfirmed exit fails closed: log and count
   the error, exit the candidate, and never bind. The controller does not signal
   process groups or arbitrary PIDs from a status endpoint.
4. Confirm activation with an unexpired owner/epoch compare-and-set. This records
   the new process as the possible forwarder **before** any socket bind.
5. Bind the original sockets, restore routes through ADR 0003, and start serving.
   The new instance token makes the control plane re-register its live workers.

The lease distinguishes its current holder from its last possible forwarder.
This matters if a standby dies after claiming but before fencing. A later claim
retains the original forwarder and also fences the interrupted candidate.
Activation only replaces that evidence after both targets have been fenced.
An interrupted activation or bind is safe because a successor must fence the
activated holder before binding. The metadata stays bounded at two predecessor
identities, not an unbounded chain of failed candidates.

## Timings and self-fencing

Defaults are a **600 ms lease**, **200 ms renewal**, and **100 ms claim poll**.
They leave room for two missed renewal intervals, bound ordinary failure
recognition to at most about 700 ms, and leave roughly 300 ms for local fencing,
route restore and control-plane re-registration within the one-second caller
budget. These are configurable with `-relay-lease-ttl`, `-relay-lease-renew` and
`-relay-lease-poll`; renewal must be positive and shorter than the lease. Loaded
hosts, a slow store or a large route scan can exceed the measured loopback gap.
The gap guarantee is established by the real-process driver, not by these
configuration values alone.

A successful renewal compare-and-set rejects any different owner or epoch.
That observed lease loss immediately disables packet forwarding, closes both
UDP sockets, cancels the HTTP service, and exits. The forwarding check is local
and nonblocking on both packet directions, including held-packet releases.
Closing packet sockets does not wait for route persistence or store cleanup.
The private status exposes the lease epoch, acquisition/fencing/activation and
bind-plus-restore durations, registered-worker count and self-fence count.

Lease expiry alone does **not** prove a successor exists. Renewal may extend an
expired tenure when it still has the same owner and epoch. While Redis is
unreachable the active relay keeps forwarding existing routes; failed owner
lookups and persistence retain their existing ADR 0003 behaviour. The standby
cannot claim or activate, does not signal the active process, and does not bind.
After Redis recovers, whichever atomic claim or renewal wins decides ownership.
There is no takeover without a successful lease decision.

## Evidence and limits

`crash-run -relay-standby` runs SIGKILL and SIGSTOP trials with the existing
whole-window caller-gap measurement. It keeps the caller, workers, control plane
and cryptographic state alive. It checks restored routes, no reconnect or
renegotiation, zero decryption failures and caller duplicate RTP counts. It
records counters from the old process before failure and the new process after
recovery, checks the old child is reaped at readiness (allowing 100 ms for its parent
to reap an already dead child), and requires
a changed instance token and increased lease epoch. The SIGSTOP trial attempts
SIGCONT after takeover through the owned-child handle; the handle reports the
old PID already reaped and never sends a signal to a potentially reused PID.
SIGCONT cannot resurrect a killed process. Timing columns separate the lease
claim, fencing, activation, socket/route startup, readiness and worker
registration. Claim, readiness and registration times start at the injected
signal; fencing, activation and bind/restore are stage durations.

Production-command tests cover a real Redis transport outage, successor-induced
self-fencing and mismatched kernel identity with no socket binds. Unit tests
cover both stores, competing claims, interrupted claimants, expired activation,
identity/signalling failures, and a stopped child killed by the identity helper.

This design is for cooperating relay binaries on one Linux or macOS host, with
permission to terminate one another, and a store whose acknowledged lease
decisions are retained. PID/start-time verification uses the platform process
query immediately before signalling; it is not a cross-host fencing primitive.
Cross-host failover, a floating public IP, durable Redis failover policy and
supervision that relaunches failed standbys remain outside this decision.
