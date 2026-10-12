# ADR 0008: Retain live ownership through a bounded store outage

Redis is outside established media forwarding. Retain worker ownership and its
snapshot for the **store outage budget plus the normal lease lifetime**. The
default budget is **30 seconds**: worker leases and snapshots last 33 seconds,
with the existing one-second renewal and 100 ms snapshot cadence. Three seconds
of headroom cover renewal phase and bounded store recovery work. Keep expiry
coupled: snapshot bytes grant no ownership. Decoupled retention would require
new recovery inventory and ownership operations without making old counters safe.

Configure `mediaworker.RelayConfig.StoreOutageBudget`,
`controlplane.Config.StoreOutageBudget`. Zero selects 30 s. Use matching
`-store-outage-budget` on worker and control. Explicit worker/control `LeaseTTL`
overrides total lifetime, including headroom; shorter values forfeit the budget.
Relay leases retain their independent 600 ms TTL and observation policy below. Worker `LeaseRenewInterval` defaults to
one second independently of TTL; explicit short TTLs retain the old TTL/3 cadence.

## Retention and renewed artifacts

| Artifact | Store outage behavior |
| --- | --- |
| Worker lease and encrypted snapshot | Coupled expiry at budget + 3 s. Transient errors retain live media/keys; authoritative renewal or checkpoint `ErrLeaseLost` still fences and closes. |
| Session epoch, transition metadata, worker indexes | Existing lease-relative record/index retention follows the longer TTL. Global epoch stays persistent. Index refresh stays on each renewal tick. Missing ownership cannot be re-claimed for resume. |
| Ended tombstone | Existing separate five-minute marker remains. Successful release is a normal end; uncommitted release can still count as expiry. |
| Relay lease and fencing evidence | Persistent fencing record; 600 ms logical TTL, 200 ms renewal, 100 ms poll. A standby needs a full TTL of consecutive successful unchanged observations; failed polls reset evidence. Matching expired-but-unclaimed renewal succeeds, so store pauses do not trigger takeover. |
| Persisted routes | Original authenticated consent deadline plus one-second hash cleanup margin, unchanged. No fabricated consent during failure. Live cached routes continue forwarding/authenticating; later checks republish after recovery. Relay restart retains ADR 0003 partial-restore limits. |
| Local relay flow/hold/replay state | Unchanged. Lookup errors preserve established routes; bounded coordination holds still time out. |
| Frame cache | Best effort, encrypted, existing 30 s idle expiry. Missing cache/write failure never fences live calls. Checkpoint policy still governs recovery. |
| Control registry, heartbeats, pending recovery, history | Local memory, unchanged. Store errors retain calls/retry state. Heartbeat death remains 400 ms plus 50 ms tick. Control restart still loses registry/history. |

Ownership sweeps schedule every `min(LeaseTTL/2, 1.5 s)` plus the detector tick.
Real expiry does not acquire a 16.5 s sweep delay. This bounds scheduling, not
total latency: an ongoing sweep, store work, call count and coordination add time.
Cleanup retry backoff remains 250 ms to 30 s; status does not perform cleanup.

No component fences on transient store errors. Session commands retain two-second
deadlines, disabled automatic network mutation retries, and three-attempt
read/idempotent retry with 20 ms initial backoff, jitter and 500 ms cap. Worker
ownership calls have two-second deadlines; relay lookup/persistence one second;
relay renewal/claim use renewal/poll deadlines. Frame writes have two-second
deadlines and worker reads are best effort (150 ms default). These are work bounds,
not permission to invent ownership. New calls, moves and takeovers wait for Redis
or fail cleanly. Normal heartbeat detection and CAS takeover do not wait for expiry.

## Exclusivity proof

1. `sessionstore.Redis.Transfer` and Memory atomically compare the old worker and
   epoch before installing the new worker and a higher epoch. Two transfers cannot
   both replace one tenure. Short expiry is not the exclusivity primitive.
2. The relay moves each confirmed flow to the successor. `flowTable.answer`
   accepts worker media only from that flow's worker. Pending STUN success must
   match an outstanding request; trusted movement discards old requests/candidates.
   Before the flow switches only A forwards; afterwards only B forwards.
3. A store-partitioned worker retains its session but cannot reach a caller whose
   relay flow moved. On restored access, renewal/checkpoint observes `ErrLeaseLost`
   and fences. It cannot re-claim the call or release a successor's tenure.
4. A standby relay still needs its own CAS, verified predecessor fencing,
   activation and exclusive sockets before forwarding (ADR 0004).

`TestPartitionedWorkerStoreTransferHasExclusiveCallerMedia` uses real workers,
detector, transfer, relay and Pion caller on both stores. Only A's store adapter
is partitioned. A retains its session/media loop. B takes a higher epoch before
A expires. Forty deliberately stale encrypted packets from A never reach the
caller's raw socket while B media continues. Restored A access fences its old
session. Passing decryption alone is not proof; raw packet absence is checked.

Verified relay process death still bypasses expiry on the next 100 ms standby
poll. A frozen holder is detected after about one 600 ms TTL of reachable
staleness, retaining #43's under-one-second loopback caller gap on defaults.
Failed store polls erase observation time; after shared store recovery the
healthy active renews before a fresh full TTL can accumulate. The relay policy
is independent of the worker retention budget and also tolerates longer shared
store outages while its persistent fencing evidence remains intact.

If only the active is partitioned, the reachable standby sees unchanged lease
evidence for a full TTL, wins the store claim and fences the old process by
verified PID/start identity before activation and binding. Observation cannot
create two forwarders: it grants no forwarding rights and cannot skip the CAS,
verified predecessor termination or exclusive socket binds. A renewed lease
rejects an expired-tenure claim; a successful claim rejects the old renewal.
The guard unit test exercises active-only partition through separate store
adapters; the default SIGSTOP driver proves real predecessor termination before
new readiness. ADR 0004 records the full ordering and platform-specific identity
checks, including Linux pidfd fencing.

## Recovery and expiry

Live sessions retain current keys/counters and never reload snapshots after an
outage. Crashed workers still pass ADR 0002's snapshot-copy age, scaled margins,
caller reserve and caps. A short pause resumes. A 5 s pause now retains ownership
long enough to count `checkpoint_unsafe`, spending no resume margins. Longer
retention does not widen the cryptographic envelope.

Thirty seconds is the supported budget, not an exact termination timer: headroom
can permit some longer pauses. After actual ownership expiry (35 s default driver
case), renewal/put cannot resurrect it; #99 counts one `ownership_expired` loss and
cleans only that tenure. Forwarding continues while Redis remains unavailable;
terminal fencing/accounting happens when authoritative access returns. Accounting
can precede asynchronous cleanup; the expiry driver separately confirms cleanup
within three seconds and rejects any media after that confirmation.

The contract assumes preserved acknowledged Redis decisions, stable store clock,
matching budgets, continued caller consent/media and live worker/relay/control.
It excludes lost store data, control or relay restart during the outage, cross-host
relay failover, unbounded host scheduling delay, and crash resume beyond ADR 0002.
