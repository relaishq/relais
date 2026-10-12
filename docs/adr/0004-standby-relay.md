# ADR 0004: Fence a same-host standby relay before binding

A relay restart loses every call's media until its sockets and routes recover.
The standalone `relais-relay` now owns a fenced relay lease. A second instance
with `-standby` waits without binding sockets and takes over the same public UDP,
worker-leg UDP and private HTTP addresses after verified process exit or lease
expiry. Both processes use the same Redis namespace and `-relay-lease` key
(default `default`). Each socket set needs a distinct key. Standby addresses must have fixed, nonzero ports.
Neither UDP socket uses `SO_REUSEPORT`; kernel socket exclusivity remains a
second defence against simultaneous forwarding.

## Lease and fencing order

Memory and Redis implement an independent `RelayLeases` capability with claim,
renew, transfer, activation, release and read. The owner is a fresh instance token, with a
monotone epoch, expiry, PID, kernel start time, host/PID-namespace markers, and
the executable device/inode captured when that process starts. The relay's keyless
`RedisOwners` capability gains these metadata operations, not snapshot access.
Redis uses one atomic Lua script and Redis `TIME`; memory uses its lease lock.
The Redis hash is persistent: expiry must not erase fencing evidence or reset an
epoch. Keep namespaces and keys stable while old processes can exist. Do not
share a relay key across hosts or PID namespaces. A missing hash can be
re-created by renewal when no successor is recorded: restore the same holder and epoch, with the
forwarder set to self. An existing different holder or newer epoch rejects
renewal. An older epoch for the same holder is repaired without decreasing the
relay's epoch. A claim racing re-creation still has one atomic winner; a losing
active observes the successor and self-fences. Redis `TIME` is a wall clock:
an NTP step on the Redis host shifts lease expiry.

Takeover follows this order:

1. Read the lease and validate every recorded predecessor's host and PID
   namespace before interpreting local PIDs. A foreign or missing scope marker
   is a configuration error: refuse the claim, log loudly with the process-local
   `configuration_errors_total` counter, and exit without signalling or binding.
   Both stores repeat this check inside every claim transaction, preventing a
   foreign record from racing the last read. Scope changes also invalidate a
   dead-holder claim's identity comparison.
   The only startup exception is a plain relay reclaiming an expired lease
   with any identity explicitly `unknown` (holder, forwarder or previous
   holder), under the single-relay assumption below.
   `ClaimExpiredUnknownRelay` atomically checks expiry, epoch and all predecessor
   evidence. A renewal after the expired read prevents the claim. It never
   bypasses expiry for a new holder, and cannot admit a foreign known-host
   forwarder or previous holder. An identical holder may settle an uncertain
   successful claim without advancing the epoch. Ordinary and dead-holder
   claims continue to refuse unknown hosts. This also permits recovery when a
   known claimant dies before activation, leaving an unknown predecessor in
   the record. Known same-host predecessors still undergo verified fencing;
   foreign known identities still refuse the claim.
   For verifiable hosts, observe both holder and last possible forwarder with the same kernel
   identity checks used for fencing, without a terminating signal.
   If both are verifiably gone, claim immediately, bypassing expiry and staleness
   observation. `ClaimDeadRelay` atomically checks the observed epoch, holder,
   forwarder and previous-holder evidence before advancing the epoch. A changed
   record rejects the claim and requires fresh observations. A renewal changing
   only expiry does not resurrect a dead process; it may complete before the
   claim, but renewals of the replaced epoch are rejected afterwards.
   Otherwise, the candidate observes the same holder, epoch, predecessor
   evidence and lease deadline across successful store polls. Only intervals
   bounded by two successful polls with that unchanged evidence accrue time,
   capped at the configured renewal interval (200 ms by default) per interval.
   A long unobserved host stall cannot supply a full TTL of staleness at once.
   Only a full lease TTL without a deadline advance, measured on the candidate's
   local monotonic clock, permits an expired-tenure claim. Observation starts
   while the lease is live, so a frozen holder is recognized about one TTL after
   its last observed renewal, rather than adding another TTL after expiry.
   A failed or timed-out poll adds no time. A short failure blip preserves
   previously accumulated staleness; intervals touching failed polls are
   uncounted. A continuous failure streak lasting at least the configured
   renewal interval (200 ms by default) resets the evidence. Recovery after
   such a streak starts fresh. An advancing deadline or changed identity/epoch
   always resets it. Sustained failure streaks longer than the renewal interval
   delay frozen-holder takeover until the store supplies a full TTL of
   reachable staleness; there is no fixed takeover bound under arbitrary flapping.
   The active normally renews within 200 ms, before the
   candidate can accumulate 600 ms of reachable staleness. Store expiry remains
   an additional claim precondition; the local observation never bypasses it.
   A renewal that wins before the claim extends expiry and the atomic claim
   refuses it. A winning claim fences all subsequent old-holder renewals.

   Fresh empty-store bootstrap and an explicitly released tenure remain
   immediate. Missing records after a store failure or an observed holder
   require a full TTL of successful absence observations, allowing the active
   to recreate its persistent record. Plain unknown-host restart retains the
   existing single-relay assumption, expiry check and identity restrictions;
   a new holder also waits for reachable staleness. An uncertain operation is
   never permission to signal or bind. Repeating a claim for the identical
   process identity settles it idempotently if that tenure still exists.
2. Start renewing the new tenure, including during fencing and route restore.
3. Fence the old lease holder and the last possible forwarder, deduplicating
   identical targets. Unknown-host predecessors in the plain startup exception
   are never probed or signalled. Linux opens a pidfd first, brackets `/proc` identity and
   executable checks with the pidfd's `fdinfo` PID, and uses `pidfd_send_signal`
   so PID reuse cannot redirect the signal. The [pidfd API](https://github.com/mkerrisk/man-pages/blob/master/man2/pidfd_open.2)
   requires Linux 5.3+ and working
   pidfd syscalls; refusal or unavailable checks fail closed. macOS retains its
   PID/start-time query and signal path, rechecking identity after its executable
   lookup. Before signalling, compare the target executable against **its
   recorded** device/inode, not the candidate's executable. Linux stats
   `/proc/<pid>/exe`, including a deleted old inode. macOS finds the main
   executable path with `PROC_PIDPATHINFO` and reads its mapped vnode device/inode
   using [`PROC_PIDREGIONPATHINFO2`](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/proc_info.c).
   It does not stat that path, which could already refer to the replacement
   binary. A standby running an upgraded inode can therefore fence a stalled
   older relay, while an unexpected executable change still fails closed.
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

The dead-holder check applies to both plain startup and every standby poll.
ESRCH, a zombie, or a start-time/boot-ID mismatch proves the recorded process
exited. No executable lookup is possible or needed for an absent/reused/dead
identity; a matching live identity undergoes the executable check and a second
identity read. Linux uses the pinned pidfd throughout. If a `/proc` identity
read fails, recheck the pidfd and probe it with signal 0; an unreadable/hidden
entry for a still-live process is an error, not proof of exit. A stopped process remains
live and does not qualify. EPERM, a failed executable check, or an unconfirmable
state also does not qualify; the candidate keeps the normal expiry path,
requiring successful unchanged observations for a full TTL before expiry takeover.
Both holder and forwarder must qualify, so a dead pending claimant cannot hide a
live predecessor. The normal post-claim fencing, activation and exclusive binds
still run before media starts, including the bounded zombie socket-release wait.

A plain restart claims a verifiably dead holder on its first successful
observation. A standby normally detects SIGKILL by the next 100 ms poll. Socket
startup, route restoration and worker registration still add time. A SIGSTOPped
holder retains a live identity: the standby requires a full TTL of successful,
accumulated unchanged lease observation time and store-confirmed expiry before
claiming it. Short poll failures do not erase successful observation intervals.

Defaults remain **600 ms lease**, **200 ms renewal**, and **100 ms poll**.
A shared Redis outage with a failure streak of at least one renewal interval
resets every standby's staleness evidence. The active
continues forwarding and retries renewal; matching holder/epoch renewal can
revive its expired but unclaimed logical lease. When Redis returns, a healthy
renewal advances the deadline before a fresh 600 ms observation can authorize
an expiry takeover. A partition seen only by the active does not protect it:
a reachable standby observes a full TTL of staleness, claims and fences it.
Both frozen-holder and dead-holder driver cases use these defaults; no short-TTL
override hides a product regression. These are local measurements, not a loaded
host or slow-store guarantee. Flags remain `-relay-lease-ttl`,
`-relay-lease-renew` and `-relay-lease-poll`; use consistent relay settings.

Observation only decides when to try the claim. It never grants forwarding.
The store serializes claim and renewal; takeover preserves holder/forwarder
identity evidence. The winner must verify and terminate every old possible
forwarder, activate its own tenure, and exclusively bind the original sockets.
A fencing failure refuses activation/binding. Thus even an active-only partition,
an observation race, or uncertain claim cannot create two forwarders. Redis must
preserve acknowledged decisions, as required by the existing lease contract.

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
if shorter). Short failed polls contribute no staleness time. A continuous
failure streak of at least one renewal interval resets the observer. On recovery
after that streak,
a full TTL of unchanged evidence is required before an expiry claim, giving a
healthy active time to renew or recreate its record. A continuously connected
candidate follows the same observation rule, so an active-only partition is
still fenced after reachable staleness. There is no takeover without a successful
store decision, verified predecessor fencing and activation.

On graceful exit, close both UDP sockets and HTTP, stop and join renewal, then
release the matching holder/epoch with a bounded store operation. Release clears
process identities and expires the tenure while retaining its epoch. A plain
restart can claim it immediately, without waiting for the old TTL or staleness
observation. A stale release cannot erase a successor. A crash or failed release
retains fencing evidence; verified exit permits the fast claim, while unknown
exit retains the expired-tenure path with full reachable-staleness observation.

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
PID. Command tests also require plain restart and a waiting standby to become
ready within 500 ms after SIGKILL despite a 3 s lease, and prove a stopped holder
is not claimed early. Unit tests cover EPERM/unknown exit, a changed holder during
the identity check, a dead holder with a live forwarder, and the claim/renew race
in both stores. Regressions also cover candidate-side recovery versus an
active-only partition, missing-hash re-creation after failed polls, foreign
host/namespace refusal before any claim, atomic store scope checks, Linux
hidden identity reads, and an upgraded executable fencing a stopped old relay.
The upgrade and foreign-scope paths run through real command/Redis tests.
Unit tests cover both stores, competing claims, interrupted claimants, expired activation,
identity/signalling failures, and a stopped child killed by the identity helper.

`-host-id` overrides machine identity discovery on Linux and macOS. Its default
is `RELAIS_HOST_ID`; an explicit flag overrides the environment. Use the same
stable value for the active and standby on one host, and distinct values for
different hosts. The recorded marker is `host-id:<value>`; an explicit value
does not bypass PID namespace, boot or executable verification.
Changing a configured ID changes the scope marker. Stop and release the active
before changing the ID for both active and standby.

Without an explicit host ID, Linux host scope uses `/etc/machine-id`, falling back to
`/var/lib/dbus/machine-id`, plus the device/inode of `/proc/self/ns/pid`.
If neither file provides an ID, a plain relay still starts with the host marker
`unknown` and logs a warning. Namespace, process start and executable identity
must still be readable. Standby mode refuses startup without a resolvable host
identity and tells the operator to set `-host-id` or `RELAIS_HOST_ID`.

An unknown-host identity is unverifiable, including against another unknown-host
record: it has no dead-holder fast path and is never killed. A resolved standby
also refuses a record containing an unknown-host identity. Only a plain relay
starting up may reclaim an expired lease containing any unknown-host identity.
If a known relay claims such a lease and crashes before activation, the next
plain relay can recover it after expiry. Known identities retain the normal
scope verification and fencing rules; they are never skipped with the unknown
identities. This is the single-relay container restart case:
the operator has stopped the only relay before starting its replacement. It
assumes no standby and no other surviving relay. Expiry does not prove that the
old process stopped; exclusive socket binds still apply. Configure a host ID
before using a standby. An unknown-host active can release gracefully to clear
its identities before starting an active and standby with the new host ID.

macOS uses the machine UUID from
[`gethostuuid`](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/sys_generic.c)
and one host PID namespace. `kern.uuid` is the
[kernel build UUID](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_sysctl.c),
so it cannot distinguish machines running the same kernel. Linux boot IDs remain
in the start identity: a different valid boot ID on the same machine proves the
old namespace and process cannot survive, even if its namespace inode changed.
Different namespaces in the same boot are foreign. An occupied legacy lease
without scope markers is unverifiable and refused. A graceful old-version
release clears those identities and permits the new record format. After a
crash, verify that all old relays have stopped before a deliberate lease-key
migration. Do not erase an old record while its process may still forward.

The host marker assumes machine IDs are unique. An image with a baked-in
machine ID running on several hosts does not separate those hosts. A holder on
another host can look like a rebooted local holder: a different Linux boot ID
then appears to prove that it is gone. Set a distinct `RELAIS_HOST_ID` per host,
or regenerate the machine ID at boot. Do not share a relay key across hosts.

This design is for cooperating relay binaries on one Linux or macOS host, with
permission to terminate one another, and a store whose acknowledged lease
decisions are retained. Identity verification uses the kernel process query
on macOS and a pinned pidfd on Linux; it is not a cross-host fencing primitive.
Unsupported platforms, including Windows, retain plain relay operation without
this process lease; `-standby` is refused with a clear error.
Cross-host failover, a floating public IP, durable Redis failover policy and
supervision that relaunches failed standbys remain outside this decision.
