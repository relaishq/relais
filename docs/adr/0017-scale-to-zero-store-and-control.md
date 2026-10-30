# ADR 0017: Suspend the session store and control at zero calls

Status: **Proposed** 2026-10-29. Part of [#279](https://github.com/relaishq/relais/issues/279)
and the [#275](https://github.com/relaishq/relais/issues/275) scale-to-zero profile.
Code baseline: `cdb48fe`. This is a design draft. No new adapter or cold-start
behavior is implemented or measured by this ADR.

## Owner summary

- Use a PostgreSQL session-store adapter for the optional profile. Neon is the
  first deployment candidate because it can suspend database compute and wake
  on a connection while retaining durable state.
- Keep the existing memory and Redis adapters. A deployment uses one authority
  store. Existing calls do not move between database engines.
- Stop control only after proving that no call or unfinished ownership operation
  remains. First-call signaling wakes it through a separate callable ingress.
- Keep control running while any call needs takeover. A controller crash with
  live calls triggers immediate supervised restart.
- At zero calls, quiesce worker and relay store traffic too. Otherwise the store
  cannot become inactive. Keep a reachable activation path.
- Reuse dial-out registration, heartbeat death detection, rejoin and relay-leg
  authentication from proposed [ADR 0015](https://github.com/relaishq/relais/pull/299).
- Propose a first-call p95 target of five seconds in the reference test network.
  This is a target for #280 to prove, not an established guarantee.
- Keep usage facts and peak simultaneous calls in the store. Export reports
  asynchronously after wake. A reporter cannot hold up first-call admission.

## Context and evidence

The session store is an ownership authority. It also holds encrypted resumable
state, checkpoint time, transition settlement, routes, relay fencing evidence
and the control restart journal. Replacing Redis requires all these contracts,
not only a key lookup and an increment.

`pkg/sessionstore/owners.go` defines atomic owner/epoch/expiry checks. Claim
refuses an existing record, including an expired record before pruning. Renew
and Transfer cannot revive an expired session. Transfer preserves its state.
Stale Release cannot delete a successor. Epochs increase after reclaim.
`checkpoint.go` requires the store's clock and the exact latest state bytes.
`redis_components.go` publishes one authenticated component tuple after its
preconditions and preparation. Unknown replies require settlement.

[ADR 0011](0011-deploy-and-autoscale.md) requires preserved acknowledged
ownership decisions. Its current deployment direction places resources in the
operator's cloud account. Choosing Neon introduces a separate managed-store
boundary. **Owner acceptance of that boundary is required before implementation.**
This proposal changes neither the existing deployment nor ADR 0011's supported
claims. [#280](https://github.com/relaishq/relais/issues/280) amends that ADR only
after the complete optional profile has evidence.

PostgreSQL row locks and transactions provide the primitives for atomic state
changes, and `clock_timestamp()` provides database time [E1–E2]. Neon documents
synchronous commit acknowledgement after a quorum of WAL safekeepers confirms
the commit record [E3]. Its compute can suspend after inactivity and reactivate
on a connection [E4–E5]. These are capability sources. None proves the Relais
adapter, latency, recovery or capacity.

## 1. One PostgreSQL authority, with unchanged safety limits

Implement a separate adapter for the session, component, route, control and
relay-lease interfaces. Supply the optional frame-cache adapter or explicitly
disable that cache for the profile; do not leave an undeclared Redis dependency.
Use the writer endpoint for authority and checkpoint reads. Require synchronous
durable commit acknowledgement. Read replicas cannot settle ownership.

Each session operation uses a short transaction. Lock the session record,
sample database time after any lock wait, check the exact owner/epoch/expiry
and expected checkpoint tuple, and publish the complete result atomically.
Keep immutable ciphertext and authenticated metadata compatible with the existing
formats. Preserve sequence reservation, content revisions, encryption-key
rotation, successful-write accounting and the full unsigned epoch range.
Database field capacity does not increase application state limits [E6].

Use database wall time conservatively. `now()` is transaction-start time and
cannot replace publication-time sampling. A timestamp sampled before durable
commit makes acknowledged age conservative; waiting for the reply cannot make
an old transport copy fresh. Retain backward-clock refusal and ADR 0002's
snapshot-copy age check across database restart/failover. Do not use worker time
as an undocumented substitute.

An uncertain commit is not a failed mutation. Preserve candidate and tuple
identifiers, fence a delayed candidate before declaring non-commit, and re-read
the writer to settle the result. Retain typed transient, lost-lease and
superseded-state outcomes. Do not add automatic retries of unknown mutations.
Logical expiry is checked during operations; background deletion is cleanup.
It does not grant ownership or remove epoch history prematurely.

Retain the [ADR 0012](0012-agent-state-store-schema.md) admission envelope:
100 ms captures, 6,144 B hot writes, 20,480 B total transactions, 70,000 B/s
and 40,960 B burst for the default split profile. Keep checkpoint-age and
snapshot-age p99 at or below 550 ms, report maxima, and preserve the individual
takeover safety checks in [ADR 0002](0002-checkpoint-age-envelope.md).
Keep the existing 100-command and 128 KiB request-plus-reply per call-second
envelope; report SQL statements and network exchanges separately so one
stored procedure does not conceal its work.

Retain [ADR 0008](0008-store-outage-budget.md): 30 s outage budget, 34 s
default worker lease/snapshot lifetime, one-second renewals and two-second
session-command bounds. Established media survives transient store faults
within the existing assumptions. New ownership operations wait or fail with a
typed outcome. A database outage never supplies authority or fresh counters.
Longer retained state does not make a stale checkpoint safe for takeover.

## 2. First signaling owns wake; control owns admission

Separate the authenticated signaling/wake ingress from the stoppable control
process. Keep its URL reachable while control is stopped. It invokes a
configured activation mechanism for the selected runtime. A raw UDP packet,
metric scrape, report retry or idle worker heartbeat must not activate control.

Persist an activation generation and pending signaling reservations. A short
transaction serializes a new reservation against the zero-call stop decision.
The ingress records the reservation before asking the runtime to start control;
concurrent first calls share activation. A lost activation reply can be retried
for that generation. The controller's existing store lease remains the single
authority gate even if the runtime starts two processes. An abandoned activation
has a bounded reservation lifetime and can be taken over; it cannot strand
future requests behind a dead launcher.

```text
authenticated signaling
          |
          v
reserve wake generation -----> activate control / relay / workers
          |                                |
          |                         claim control lease
          |                         rebuild and reconcile
          |                                |
          +------ wait for readiness <------+
                         |
                    normal admission
                         |
                first decrypted media
```

A reservation is not a created call. Keep it until normal create reconciliation
establishes the result. Repeated delivery for one signaling attempt must not
issue a second uncertain create. Use the durable create journal and a stable
request identity; never turn an HTTP retry into blind replay of a worker effect.
Bound queued requests and total activation work.

Run the existing restart sequence on every wake: claim control authority,
load configured registrations, reconcile durable calls, routes, operation
intents and uncertain creates, cross-check worker inventory, and complete rejoin
before admitting mutations. Persisted address data and missing status do not
prove liveness. Keep local control-lease deadlines based on request start.

While at least one call, reservation, move, takeover or cleanup exists, keep
control and its supervisor active. An unexpected control exit restarts through
that supervisor, even if no new signaling arrives. Cold start after idle is
an additional entry to restart recovery; it does not replace active-call
recovery or add a wake delay to the worker-crash takeover target.

## 3. Zero calls is a proof, not a sampled metric

Do not stop from an in-memory call count, an expired worker heartbeat or an
unavailable store read. The idle transaction must establish all of these:

- No authoritative admitted call remains, including a call whose create reply
  was lost. Expired records have a settled terminal outcome.
- No incoming reservation, create journal entry, transition candidate,
  planned-move recovery, takeover or terminal cleanup remains unresolved.
- In-flight commands are completed or settled and cannot later publish a call.
  Workers report empty inventories and no incoming assignment.
- The activation generation has no newer signaling reservation. Admission is
  closed for the retiring generation before quiescence begins.

First-call reservation and the final idle commit use the same deployment gate.
If signaling wins, cancel idle stop. If idle wins, signaling records a new
generation and wakes a successor. A retiring controller cannot release a new
controller's lease. A wake racing the retirement either keeps the current
generation active or waits for the successor to finish restart reconciliation.
Unknown state retains the active controller.

After this proof, coordinate worker and relay quiescence, recheck the generation,
stop and join control work, release its exact control tenure, and exit. Preserve
epoch history, ended markers, usage facts and reporting progress. Pending report
delivery does not prevent suspension. The separate ingress remembers the idle
generation; after its own restart, unknown state requests normal reconciliation
instead of assuming the deployment is empty.

## 4. Dial-out workers and relays during idle and wake

Proposed ADR 0015 keeps a worker's command stream separate from heartbeats.
A lost command link alone never establishes death. While calls exist, retain
the existing 400 ms heartbeat detector, 50 ms tick, fenced Transfer, rejoin
tokens and target eligibility rules. No idle optimization applies to those
calls. Worker store renewals and checkpoints continue independently of control.
Python bridge connections retain ADRs 0014/0015's worker-to-runner and
worker-to-bot direction; control is not their media rendezvous.

After the idle proof, empty workers close their command links and stop
heartbeats, store maintenance and relay-leg keepalives. They may exit. A retained
empty worker uses the activation ingress's idle generation response, which
requires no periodic database reads. Heartbeat/link requests do not repeatedly
wake a sleeping controller. New demand activates workers or notifies retained
workers through that ingress. On wake, repeat the authenticated hello,
instance/death-cycle reconciliation, leg-token exchange and inventory checks.
Do not reuse a process's former eligibility, token or NAT address without them.

The relay group must also become inactive. Tell every standby to stop observing
before releasing the active relay's lease. Obtain acknowledgements, stop and
join lease and lookup work, disable forwarding and close packet sockets, then
release the matching relay tenure while retaining fencing history. An
unresponsive possible forwarder or standby prevents the idle proof. No standby
may interpret intentional quiescence as a frozen holder and reacquire it.

Keep the relay address reserved and a private activation endpoint reachable.
That endpoint can remain in a dormant process; it does not forward packets or
query the store. This ADR does not require relay-host suspension or a new public
address. Address replacement and ICE restart remain
[#278](https://github.com/relaishq/relais/issues/278) work.

On wake, reactivate the relay group through the normal
[ADR 0004](0004-standby-relay.md) claim, predecessor fencing, activation,
exclusive socket bind and route checks. The worker repeats its dial-out leg
handshake. Control admits the call only after worker and relay readiness.
Never extend consent, forward under an idle marker, or bypass fencing to meet
the first-call target. Retain the 600 ms relay lease, 200 ms renew and 100 ms
poll policy whenever the relay group is active.

No background database probe, pool health query, relay poll or usage exporter
runs while the deployment is idle. Close idle transactions. Neon suspends only
after its inactivity interval; a direct operator metric query can wake the
database without starting control [E4–E5]. This is not a claim that all retained
processes or infrastructure disappear.

## 5. Cold first-call budget

Propose **p95 ≤5 s** from authenticated first WHIP request accepted by ingress
to the first correctly decrypted returned media in the reference network.
Use an overall **10 s request deadline**; return a typed startup-unavailable
outcome rather than hanging or reporting success without a call.

| Stage | Proposed planning allowance |
| --- | --- |
| Database wake, control start, authority and reconciliation | 2 s combined |
| Worker and relay activation, hello/rejoin and readiness | 2 s; activate in parallel where safe |
| Signaling completion and first returned media | 1 s |

These allowances allocate the target, not separate provider guarantees.
Report p50/p95/p99, maxima, failures and each stage. Include first use after
five minutes, 24 hours and more than seven days of idle; Neon documents longer
activation and cold-buffer effects after long inactivity [E5]. First confirm
database readiness, then capture resumable transport state. A database wake
delay must not make an old pre-wake capture appear fresh.

Do not widen checkpoint age, store outage, active takeover, command or consent
limits to hide startup tails. #277 and #278 may establish that a selected
worker/relay runtime cannot meet this allowance. #280 must then bring measured
options back for owner decision before declaring a supported profile.

## 6. Durable usage facts, direct peak reads and asynchronous reports

Define simultaneous calls as distinct admitted, nonterminal call IDs within a
configured reporting window. Incoming reservations are reported separately.
Transfer, worker replacement and control restart do not create an extra call.
End and terminal loss each remove a call once. Retain peak values across idle;
a window rollover does not delete the preceding result.

Persist idempotent lifecycle facts, the active membership and window identity
in the authority store. Capture them with the existing durable lifecycle
transition so a lost controller response cannot lose or duplicate the fact.
Keep a stable event identity, session incarnation and sequence. The peak is
derived from the ordered lifecycle facts or a reconciled store projection,
not from resetting process counters. A direct store query must return it with
the window, observation boundary and completeness state while control is absent.

Keep optional reporting projection and delivery out of ownership locks and
first-call waits. If a peak projection is contended or unavailable, read the
durable facts or return its explicit incomplete boundary. Do not wait for a
report destination, replay backlog or metric rebuild before the first call.
Durable ownership being unavailable still prevents admission under the existing
contract; report delivery being unavailable does not.

Before idle exit, leave unsent events queued in the store. While control is
stopped, these facts and the previous peak remain readable; no admitted call
can begin without waking control. On wake, export in a bounded background
task after the admission path is available. Use at-least-once delivery with
stable IDs; remove an event only after destination acknowledgement. A send
whose acknowledgement is lost can be repeated with the same ID.

Set explicit queue size/retention bounds. Reporter timeout, queue pressure or
projection failure fails open for calls, with an observable gap or incomplete
window if evidence cannot be retained. Do not silently trim events and claim
an exact peak or complete report. Reporting cannot change leases, declare a
worker dead, delay takeover or keep control awake indefinitely. An optional
explicit flush may wake control; routine retries wait until the next activation.

## Alternatives and consequences

| Alternative | Reason to defer |
| --- | --- |
| Upstash Redis | EVAL/MULTI/TIME exist, but its detailed consistency contract describes eventual consistency and deprecated strong consistency [E7]. Command compatibility alone does not preserve fencing through failover. |
| ElastiCache Serverless Valkey | Supports Redis scripting, but AWS excludes Serverless from its durable-log feature [E8]. It does not inherit the node-based synchronous acknowledgement contract. |
| DynamoDB on-demand | Conditional transactions and strong base-table reads are suitable primitives. Its documented condition language supplies no store-time function, and TTL cleanup is asynchronous [E9–E11]. Preserving checkpoint time needs a separately reviewed design. |
| Existing Redis deployment | Retain as the established option. Continuous coordination and observation queries do not provide the new idle lifecycle. |
| Aurora PostgreSQL Serverless | Compatible SQL primitives, but documented typical resume is about 15 s [E12], beyond the proposed first-call target before other activation work. |

The SQL adapter is substantial work. Database suspension loses session-local
objects, so correctness must live in durable rows, not session advisory locks,
temporary tables or notifications [E5]. Build a bounded adapter proof before
deployment integration. Do not add a second source of ownership or live
cross-engine migration. Adapter/schema capability and profile compatibility
must be explicit in release checks.

## Implementation acceptance

1. **Store contract.** Run the existing shared `pkg/sessionstore` suite under
   the new adapter with unchanged assertions. Cover ownership, prune/reclaim,
   unsigned epochs, stale release, tombstones, component CAS/publication,
   key rotation, exact acknowledgement, clock rollback, unknown replies and
   delayed-candidate settlement. Redis-specific key/script/wire tests remain
   Redis tests; SQL gets equivalent isolation, failure and traffic tests.
   Cover control and relay leases, route consent and the chosen cache mode.
2. **Cold restart.** Run the existing control-restart/readiness and process
   restart cases from a new control process and inactive database, for inbound,
   dial-out and mixed pools. Retain readiness gates, uncertain create
   reservations, inventory cross-checks, slow/unknown-worker handling,
   request-start authority deadlines and stale rejoin rejection. Repeat active
   control death with a worker down and with an uncertain transfer pending.
3. **Idle races.** Deterministically interleave stop with first reservation,
   an uncertain create, delayed ownership publication, simultaneous wakes,
   launcher death, gateway restart and relay standby observation. Prove one
   controller authority, no stranded call, no false takeover and no store
   polling after quiescence for longer than the database inactivity interval.
4. **Safety and timing.** Preserve existing move, drain, worker-crash takeover,
   relay standby, consent, raw stale-packet absence and zero-decryption-failure
   gates. Test 5/15/29/35 s store failures and acknowledged-write failover.
   Measure complete checkpoint paths, both age p99 values and maxima under
   admitted load. Existing store operation deadlines remain unchanged.
5. **First call.** Measure the stated end-to-end and stage budgets over repeated
   independent cold starts, including long idle. Report unavailable starts and
   tail failures; no exclusion because a worker or relay was slow.
6. **Usage.** Read the previous window and peak directly while control is
   stopped. Race overlapping calls, duplicate lifecycle delivery, end/loss,
   transfer, restart and window rollover. Block the reporter, lose send
   acknowledgements and fill its queue. Calls still start within the same
   budget; duplicate IDs are stable and incomplete evidence is visible.

**NOT-RUN — adapter implementation, shared-suite adapter acceptance, managed
failover, cold restart, first-call timing and usage-delivery fault tests.**

## External evidence

All sources accessed **2026-10-29**. Provider capabilities do not establish
Relais performance or fault acceptance.

- **E1** [PostgreSQL row locking](https://www.postgresql.org/docs/current/explicit-locking.html).
- **E2** [PostgreSQL database time functions](https://www.postgresql.org/docs/current/functions-datetime.html).
- **E3** [Neon safekeeper commit acknowledgement protocol](https://github.com/neondatabase/neon/blob/main/docs/safekeeper-protocol.md).
- **E4** [Neon scale to zero](https://neon.com/docs/introduction/scale-to-zero).
- **E5** [Neon compute lifecycle](https://neon.com/docs/introduction/compute-lifecycle).
- **E6** [PostgreSQL field limits](https://www.postgresql.org/docs/current/limits.html).
- **E7** [Upstash consistency model](https://upstash.com/docs/redis/features/consistency).
- **E8** [ElastiCache durability limitations](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Limitations.html).
- **E9** [DynamoDB conditional write operations](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/read-write-operations.html).
- **E10** [DynamoDB condition-expression grammar](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/Expressions.OperatorsAndFunctions.html). The missing store-time primitive is an inference from the documented API.
- **E11** [DynamoDB TTL behavior](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/TTL.html).
- **E12** [Aurora pause and resume](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-serverless-v2-auto-pause.html).
