# ADR 0017: Scale-to-zero control and the store seam

Status: **Proposed** 2026-10-29. Revised 2026-11-02 (owner decision: store outside scale to zero; store seam with two guarantee levels).
Part of [#279](https://github.com/relaishq/relais/issues/279) and the
[#275](https://github.com/relaishq/relais/issues/275) scale-to-zero profile.
Code baseline: `9496c10`. This is a design draft. This ADR implements no new
adapter or cold-start behavior. First-call targets are unmeasured.

**Amends [ADR 0011 §4](0011-deploy-and-autoscale.md#4-one-preview-control-process-and-a-durable-managed-store).**
The reference deployment keeps the full guarantee. Weaker-level deployments
are outside ADR 0011's supported safety claims. They do not change the
reference deployment's store requirements.

## Owner summary

- Scale to zero covers control, workers and relay. The session store stays on.
- The operator chooses the store behind `pkg/sessionstore`. Store choice sets
  the guarantee level during the store's own failover.
- Stop control only after proving zero calls and no unfinished ownership work.
  A separate signaling ingress wakes it. A crash with live calls triggers
  immediate supervised restart.
- Quiesce empty workers, relay and standby. Keep a reachable activation path.
  Reuse [ADR 0015](0015-worker-dial-out.md) hello and rejoin.
- Propose first-call p95 ≤ 5 s in the reference test network. This is a target
  for [#280](https://github.com/relaishq/relais/issues/280), not a measured result.
- Keep usage facts and peak simultaneous calls in the store. Report them
  asynchronously. Reporting cannot delay first-call admission.

## 1. Store seam and guarantee levels

The operator can use self-hosted Redis/Valkey, managed Redis, or a serverless
Redis-compatible service such as Upstash, subject to the requirements below.
A deployment uses one authority
store. Existing calls do not move between store engines. Keep the existing
memory and Redis adapters. Relais does not build a PostgreSQL adapter now;
the interface stays open to a future adapter, such as Neon, under the same bar.

The session store owns leases, epochs, encrypted resumable state, checkpoint
time, transition settlement, routes, relay fencing and the control restart
journal. Command compatibility alone does not prove these contracts.

### Required at both levels

Every Relais command, including reads and `TIME`, must use the key's single
current primary. This includes plain reads outside scripts. Inventory scans
must use current primaries only. A service that cannot guarantee this is not
compatible at either level. Causal reads on one connection are not sufficient.

The store must not evict Relais state. Use `noeviction` for Redis/Valkey.

### Full guarantee

The store must provide all four properties:

1. Atomic compare-and-swap for ownership changes.
2. The clock of the key's primary, with bounded skew across failover, for lease
   expiry and checkpoint age. Backward-clock refusal alone does not catch a
   small backward step after failover.
3. Durable acknowledged writes that survive the store's own failover.
4. Latency that meets [ADR 0012](0012-agent-state-store-schema.md): both
   checkpoint-age and snapshot-age p99 ≤ 550 ms under admitted load.

An example is Redis/Valkey with one primary, `noeviction` and AOF persistence
with `appendfsync always`, or an equivalent acknowledged-durability wait.
Do not promote an asynchronous replica. Verify all four properties in the
selected deployment.
This level preserves the acknowledged ownership decisions required by
[ADR 0011](0011-deploy-and-autoscale.md).

### Compatible, weaker during provider failover

This level must still meet properties 1, 2 and 4, the primary-only command
requirement and no eviction. Only property 3 may be absent. Normal operation
is unchanged only when these requirements hold.

Ordinary self-hosted or managed Redis/Valkey with asynchronous replicas and
replica-promotion failover is an example. Upstash is a candidate only if it
meets the common requirements. Serverless Redis services that exclude durable
acknowledgement are also candidates under those requirements.

During the store's own failover, any acknowledged write in the loss window can
be lost. This includes:

- Session ownership, control leases and relay leases.
- Routes, checkpoints and component roots.
- The create journal and control intents.
- Wake reservations and the idle commit.
- Ended markers and usage facts.

The main exposure is calls with a move, takeover or create in the loss window.
Two workers may both believe they own a call. Transient errors during the
failover do not fence ([ADR 0008](0008-store-outage-budget.md)). After the store
recovers, the losing owner fences at its next renewal or checkpoint. The default
renewal interval is about 1 s. An affected call may glitch or drop.
A just-recorded first call can fail as unavailable. The effect on calls whose
checkpoints roll back without a move is not yet analysed;
[#352](https://github.com/relaishq/relais/issues/352) tracks that work.

[Upstash documents asynchronous replication and eventual consistency](https://upstash.com/docs/redis/features/consistency).
Reads may use any replica. Causal consistency applies only within one
connection. This does not establish the required primary-only command behavior.
[ADR 0011 §4](0011-deploy-and-autoscale.md#4-one-preview-control-process-and-a-durable-managed-store)
records a managed service's serverless durability exclusion. The rollback
scenarios above are consequences of lost acknowledged writes, not measured
provider tests.

### Adapter acceptance

Every adapter must pass the existing shared `pkg/sessionstore` test suite
without relaxing assertions. Preserve owner/epoch/expiry checks, exact state
acknowledgement, store-clock checks, stale-release safety, component publication
and settlement of unknown outcomes. Passing the suite does not prove provider
failover durability or latency. Serverless Redis compatibility testing is later
work in [#352](https://github.com/relaishq/relais/issues/352). It must verify
primary-only routing for every command, including plain reads, scans and
`TIME`, along with clock skew and no eviction.

Keep ADR 0012's state and traffic limits. Keep
[ADR 0002](0002-checkpoint-age-envelope.md)'s individual takeover safety checks
and [ADR 0008](0008-store-outage-budget.md)'s outage policy: a 30 s budget,
34 s default worker lease/snapshot lifetime, one-second renewals and two-second
session-command bounds. A store outage grants no authority or fresh counters.
Longer retention does not make an old checkpoint safe for takeover.

## 2. First signaling owns wake; control owns admission

Keep authenticated signaling/wake ingress separate from stoppable control.
Its URL stays reachable and invokes the configured runtime activation mechanism.
Raw UDP packets, metric scrapes, report retries and idle worker heartbeats do
not activate control.

Persist an activation generation and pending signaling reservations. Serialize
a new reservation against the zero-call stop decision in the store. Record it
before starting control. Concurrent first calls share activation. A lost
activation reply can be retried for that generation. The existing control lease
remains the authority gate even if the runtime starts two processes. Bound
reservation lifetime so a dead launcher cannot strand future requests.

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

A reservation is not a created call. Retain it until normal create
reconciliation establishes the result. Use the durable create journal and a
stable request identity. An HTTP retry must not replay an uncertain worker
effect. Bound queued requests and total activation work.

On every wake, claim control authority, load configured registrations and
reconcile calls, routes, operation intents and uncertain creates. Cross-check
worker inventory and complete rejoin before admitting mutations. Persisted
addresses and missing status do not prove liveness. Keep local control-lease
deadlines based on request start.

Keep control and its supervisor active while any call, reservation, move,
takeover or cleanup remains. An unexpected control exit triggers immediate
supervised restart, even without new signaling. Idle wake uses restart recovery;
it adds no wake delay to active-call worker-crash takeover.

## 3. Zero calls is a proof, not a sampled metric

Do not stop from an in-memory count, an expired heartbeat or a failed store
read. The idle transaction must establish all of these:

- No authoritative admitted call remains, including one with a lost create
  reply. Expired records have a settled terminal outcome.
- No reservation, create journal entry, transition candidate, planned-move
  recovery, takeover or terminal cleanup remains unresolved.
- In-flight commands are completed or settled and cannot later publish a call.
  Workers report empty inventories and no incoming assignment.
- No newer signaling reservation exists for the activation generation.
  Admission is closed for the retiring generation before quiescence starts.

First-call reservation and the final idle commit use the same deployment gate.
If signaling wins, cancel idle stop. If idle wins, signaling records a new
generation and wakes a successor. A retiring controller cannot release its
successor's lease. A racing wake keeps the current generation active or waits
for successor reconciliation. Unknown state keeps control active.

After the proof, coordinate worker and relay quiescence. Recheck the generation,
stop and join control work, release its exact tenure and exit. Preserve epoch
history, ended markers, usage facts and reporting progress. Pending reports do
not prevent idle exit. After an ingress restart, unknown state requests normal
reconciliation instead of assuming the deployment is empty.

## 4. Workers, relay and standby during idle and wake

ADR 0015 separates commands from heartbeats. A lost command link alone never
proves death. With live calls, retain the 400 ms heartbeat detector, 50 ms tick,
fenced Transfer, rejoin tokens and target eligibility rules. Worker renewals
and checkpoints continue independently of control. Keep the bridge directions
of ADR 0014 as amended by ADR 0015. Worker-to-runner applies only to dial-out
workers.

After the idle proof, empty workers close command links and stop heartbeats,
store maintenance and relay-leg keepalives. They may exit. A retained worker
learns of idle and wake only through its own outbound connection to activation
ingress, or the runtime restarts it. It accepts no inbound connections for this. Heartbeats and link requests do not repeatedly wake control.
On wake, repeat authenticated hello, instance/death-cycle
reconciliation, leg-token exchange and inventory checks. Do not reuse former
eligibility, tokens or NAT addresses without these checks.

Tell every relay standby to stop observing before releasing the active relay
lease. Obtain acknowledgements, stop and join lease and lookup work, disable
forwarding and close packet sockets. Then release the matching tenure and
retain fencing history. An unresponsive possible forwarder or standby blocks
idle completion. A standby must not treat intentional quiescence as a frozen
holder and reacquire the lease.

Keep the relay address reserved and a private activation endpoint reachable.
A dormant process can serve activation without forwarding packets. This ADR
does not require relay-host suspension or a new public address. Address
replacement and ICE restart remain [#278](https://github.com/relaishq/relais/issues/278)
work.

On wake, use [ADR 0004](0004-standby-relay.md)'s claim, predecessor fencing,
activation, exclusive socket bind and route checks. Repeat the worker's
dial-out leg handshake. Admit calls only after worker and relay readiness.
Never extend consent or bypass fencing to meet the first-call target. Retain
the active relay's 600 ms lease, 200 ms renew and 100 ms poll policy.

Idle components stop store traffic because it serves no call. Idle commands also
add load and metered usage on the store. Holding no lease while idle keeps wake on the
ADR 0004 claim path. The store stays on. Store inactivity is not an idle condition;
operator queries can continue while control, workers and relay are stopped.

## 5. Cold first-call budget

Propose **p95 ≤ 5 s** from the authenticated first WHIP request accepted by
ingress to the first correctly decrypted returned media in the reference
network. Use a **10 s request deadline**. Return a typed startup-unavailable
outcome on failure.

| Stage | Proposed planning allowance |
| --- | --- |
| Control start, store authority and reconciliation | 2 s combined |
| Worker and relay activation, hello/rejoin and readiness | 2 s; activate in parallel where safe |
| Signaling completion and first returned media | 1 s |

These are unmeasured planning allowances. Report p50/p95/p99, maxima, failures
and each stage. Include first use after five minutes, 24 hours and more than
seven days of idle. Confirm store readiness before capturing resumable transport
state. A slow store reply cannot make an old capture fresh.

Keep checkpoint age, store outage, active takeover, command and consent limits.
If the selected worker or relay runtime cannot meet the allowance, #280 must
bring measured options to the owner before declaring a supported profile.

## 6. Usage facts and asynchronous reports

Count distinct admitted, nonterminal call IDs within a configured window.
Report incoming reservations separately. Transfer, replacement and restart do
not add calls. End and terminal loss each remove a call once. Retain peak
values across idle and preserve the preceding window after rollover.

Persist idempotent lifecycle facts, active membership and window identity with
the durable lifecycle transition. Retain stable event IDs, session incarnation
and sequence. Derive the peak from ordered facts or a reconciled store
projection. A direct store query returns the peak, window, observation boundary
and completeness state while control is stopped. These facts have the selected
store's guarantee level; do not claim completeness after lost writes.

Keep reporting projection and delivery out of ownership locks and first-call
waits. If a peak projection is unavailable, read durable facts or return an
explicit incomplete boundary. Ownership unavailability still blocks admission;
report delivery unavailability does not.

Leave unsent events queued in the store at idle exit. After wake, export in a
bounded background task once admission is available. Use at-least-once delivery
with stable IDs. Remove an event only after destination acknowledgement.
A lost acknowledgement can cause a repeat with the same ID.

Bound queue size and retention. Reporter timeout, queue pressure or projection
failure must not block calls. Expose gaps and incomplete windows when evidence
cannot be retained. Reporting cannot change leases, declare workers dead, delay
takeover or keep control awake indefinitely. An explicit flush may wake control;
routine retries wait for the next activation.

## Implementation acceptance

- **Store.** Pass the shared suite with unchanged assertions. Cover ownership,
  prune/reclaim, unsigned epochs, tombstones, components, key rotation, clocks,
  unknown replies and delayed-candidate settlement. Cover control/relay leases,
  route consent and the selected cache mode. Separately test acknowledged-write
  failover against the declared guarantee level. #352 covers serverless testing.
- **Restart and idle races.** Test inbound, dial-out and mixed pools. Retain
  readiness, inventory checks, uncertain creates, authority deadlines and stale
  rejoin rejection. Race stop with reservations, delayed publication, concurrent
  wakes, launcher death, ingress restart and standby observation. Prove one
  controller authority, no stranded call and no false takeover. Test control
  death with a worker down and with an uncertain transfer pending.
- **Safety and timing.** Retain move, drain, worker-crash takeover, relay standby,
  consent, raw stale-packet absence and zero-decryption-failure gates. Test
  5/15/29/35 s store faults. Measure both age p99 values and maxima under load.
  Measure repeated cold starts and stage budgets, including long idle and failures.
- **Usage.** Read peaks while control is stopped. Race overlapping calls,
  duplicate events, end/loss, transfer, restart and window rollover. Block the
  reporter, lose acknowledgements and fill the queue. Admission keeps its budget;
  repeated IDs stay stable and incomplete evidence stays visible.

**NOT-RUN — new adapter acceptance, provider failover, idle/wake recovery,
first-call timing and usage-delivery fault tests.**
