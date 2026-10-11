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
renew, transfer, activation, release and read. The owner is a fresh instance token, with a
monotone epoch, expiry, PID and kernel start time. The relay's keyless
`RedisOwners` capability gains these metadata operations, not snapshot access.
Redis uses one atomic Lua script and Redis `TIME`; memory uses its lease lock.
The Redis hash is persistent: expiry must not erase fencing evidence or reset an
epoch. Keep namespaces and keys stable while old processes can exist. Do not
share a relay key across hosts. A missing hash can be re-created by renewal
when no successor is recorded: restore the same holder and epoch, with the
forwarder set to self. An existing different holder or newer epoch rejects
renewal. An older epoch for the same holder is repaired without decreasing the
relay's epoch. A claim racing re-creation still has one atomic winner; a losing
active observes the successor and self-fences. Redis `TIME` is a wall clock:
an NTP step on the Redis host shifts lease expiry.

Takeover follows this order:

1. Observe the same expired holder and epoch on two polls separated by at least
   one renewal interval, then claim it, advancing its epoch. The store reports
   expiry using its own clock; a client's wall clock never authorizes a claim.
   A live tenure, changed holder/epoch, or failed read resets that observation.
   Bootstrap and an explicitly released tenure can be claimed immediately.
   An uncertain store result is
   never permission to signal or bind. Repeating a claim for the identical
   process identity settles it idempotently if that tenure still exists.
2. Start renewing the new tenure, including during fencing and route restore.
3. Fence the old lease holder and the last possible forwarder, deduplicating
   identical targets. Linux opens a pidfd first, brackets `/proc` identity and
   executable checks with the pidfd's `fdinfo` PID, and uses `pidfd_send_signal`
   so PID reuse cannot redirect the signal. The [pidfd API](https://github.com/mkerrisk/man-pages/blob/master/man2/pidfd_open.2)
   requires Linux 5.3+ and working
   pidfd syscalls; refusal or unavailable checks fail closed. macOS retains its
   PID/start-time query and signal path, rechecking identity after its executable
   lookup. Before signalling, require the target executable to have the same
   inode as our own: Linux checks `/proc/<pid>/exe`; macOS obtains the kernel
   executable vnode path with
   [`PROC_PIDPATHINFO`](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/proc_info.c). This requires both cooperating
   relays to run the same executable artifact, including during upgrades.
   Then send `SIGKILL` and wait for exit. A zombie confirms exit of the
   inspected task;
   it does not prove that all shared socket references have drained. Linux reads
   `/proc/<pid>/stat`; macOS reads `kern.proc.pid` through
   `golang.org/x/sys/unix`. A missing process is already fenced. A start-time
   mismatch proves the recorded process exited: send no signal and proceed,
   even when its PID is our own PID. Linux identities include the kernel
   `boot_id` as well as start ticks, preventing a reboot from producing an exact
   old identity. A relay may run as PID 1. An exact current-process identity is
   refused; a permission failure, executable mismatch or unconfirmed exit fails
   closed: log and count the error, exit the candidate, and never bind.
   The controller does not signal
   process groups or arbitrary PIDs from a status endpoint.
4. Confirm activation with an unexpired owner/epoch compare-and-set. Retry
   transient or uncertain activation results while continuing renewal, until
   activation succeeds, ownership is lost, or the process is cancelled. This
   records the new process as the possible forwarder **before** any socket bind.
5. Bind the original sockets with a shared, bounded retry budget, restore routes
   through ADR 0003, and start serving.
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
An expired tenure needs a further 200 ms recovery grace, so ordinary failure
recognition can take about 900 ms with poll rounding. This reduces the time
left for fencing, route restore and worker re-registration within the one-second
caller budget. These are configurable with `-relay-lease-ttl`,
`-relay-lease-renew` and
`-relay-lease-poll`; renewal must be positive and shorter than the lease. Loaded
hosts, a slow store or a large route scan can exceed the measured loopback gap.
The gap guarantee is established by the real-process driver, not by these
configuration values alone.

A renewal compare-and-set rejects a different holder or newer epoch.
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
While renewal fails, retry it every 50 ms (or the configured renewal interval
if shorter). After Redis recovers, the two-poll recovery grace gives the healthy
active time to renew before a standby can claim an expired tenure. This favors
recovery but does not promise that a stalled active wins; the atomic claim or
renewal still decides ownership. There is no takeover without a successful
lease decision.

On graceful exit, close both UDP sockets and HTTP, stop and join renewal, then
release the matching holder/epoch with a bounded store operation. Release clears
process identities and expires the tenure while retaining its epoch. A plain
restart can claim it immediately, without waiting for the old TTL or recovery
grace. A stale release cannot erase a successor. A crash or failed release
retains fencing evidence and uses the normal expiry path.

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

Socket binds retry only `EADDRINUSE`, at 5 ms intervals, under one deadline for
HTTP and both UDP sockets. `-relay-bind-timeout` defaults to 1 s and must be
positive. The lease continues renewing during that wait, and observed lease loss
cancels the wait. HTTP binds first but does not serve; packet loops start only
after both UDP sockets bind and route restore completes. An exhausted budget or
another bind error releases partial binds, logs `bind_failures=1`, and exits
without forwarding. `bind_wait_ns` and the driver's `bind_wait_ms` record the
occupied-port wait separately from total bind-plus-restore time. This budget
bounds failure; it does not extend the one-second successful caller-gap gate.

Linux CI disproved the earlier immediate socket-release assumption for an
unreaped Go child. The leading explanation is that the thread-group leader can
be a zombie while other exiting runtime threads still reference shared files.
The Linux kernel [exit path](https://github.com/torvalds/linux/blob/v6.11/kernel/exit.c)
allows a zombie leader with remaining threads, and
[file-table teardown](https://github.com/torvalds/linux/blob/v6.11/fs/file.c)
closes files when the last shared table reference is dropped. Per-task file
cleanup runs before zombie publication;
[deferred file release](https://github.com/torvalds/linux/blob/v6.11/fs/file_table.c)
is another possible source of delay. The exact CI mechanism remains unconfirmed.
The unreaped-child test now uses the production bind retry and logs owned
`/proc` thread/descriptor/children and `ss` evidence if its first bind is occupied.
It requires both sockets to bind within the shared budget before parent reaping.

Production-command tests cover a real Redis transport outage, successor-induced
self-fencing, Redis key deletion with continued bidirectional UDP forwarding,
store outage recovery without killing the healthy active, graceful SIGTERM
release, and mismatched kernel identity recovery without signalling the reused
PID. Unit tests
cover both stores, competing claims, interrupted claimants, expired activation,
identity/signalling failures, and a stopped child killed by the identity helper.

This design is for cooperating relay binaries on one Linux or macOS host, with
permission to terminate one another, and a store whose acknowledged lease
decisions are retained. Identity verification uses the kernel process query
on macOS and a pinned pidfd on Linux; it is not a cross-host fencing primitive.
Unsupported platforms, including Windows, retain plain relay operation without
this process lease; `-standby` is refused with a clear error.
Cross-host failover, a floating public IP, durable Redis failover policy and
supervision that relaunches failed standbys remain outside this decision.
