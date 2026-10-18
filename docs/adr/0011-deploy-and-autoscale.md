# ADR 0011: Deploy into your own cloud and scale by moving calls

Status: **Proposed** 2026-10-17 — awaiting owner approval. Part of #171.
Baseline: `1b952f65f0866e42a79fb8b1fd091328bff05b7a` (v0.2.0). All cloud behavior below is proposed unless explicitly marked measured.

## Owner summary

- AWS creates Relais in the customer's own account from a public link.
- The preview runs six machines and two database nodes even with no calls. Spare workers let calls move instead of drop.
- A deploy moves calls off a worker. People do not need to hang up first.
- Keep automatic scaling off until we measure capacity. Set a hard limit on each worker.
- If the relay machine fails in the preview, calls drop until it is replaced. Keeping its public address does not fix that.
- Store call ownership in a managed AWS database that keeps every confirmed write when it switches to a replacement. The usual setting can lose recent writes and leave two workers thinking they own a call.
- Ship the preview with one control process, which places and moves calls. Improve its restart recovery after the preview.
- The preview trusts its private network. Internal servers must prove their identity to each other before general release.
- Publish the cost of all running machines, spare room, the database and network traffic.
- This design approves no spend. Each cloud test needs separate approval and a cost cap.

## Scope and evidence boundary

“Serverless feel” means unattended worker replacement and scaling while calls remain connected; it does not mean scale to zero, free
idle capacity, uninterrupted provider generation, or zero media gaps. A **ready worker** has completed registration, version/agent
checks and its rejoin handshake. A **drained worker** has no local sessions, incoming reservations, authoritative ownership or
pending transfers. An HTTP success or a stopped process alone does not prove a successful drain. An **Availability Zone (AZ)** is an
isolated location within an AWS region [E37]. **Fencing** is a check that stops an old server from acting after a new one
takes over. **Synchronous durability** means saving each confirmed database write safely before acknowledging it. **Cluster mode**
requires a client that discovers database nodes and follows key-slot redirects.

Tracked evidence is authoritative within its stated topology:

| Baseline evidence | What it supports; what it does not |
| --- | --- |
| [ADR 0003](0003-persisted-relay-routes.md), [ADR 0004](0004-standby-relay.md) | Restore authenticated routes at fixed addresses; fence a same-host predecessor. No cross-host lease takeover. |
| [ADR 0005](0005-relay-buffer-replay.md), [ADR 0009](0009-planned-move-export-replay.md) | Bounded ciphertext recovery and exact planned export. Relay replacement loses the local ring/holds; uncertain replies and eviction retain documented limits. |
| [ADR 0007](0007-private-route-tcp-recovery.md), [move latency](../dev/move-latency.md), [network profiles](../dev/network-profiles.md) | Private-route tuning and shaped-network gates. Namespace measurements are not AWS latency measurements. |
| [ADR 0008](0008-store-outage-budget.md), [ADR 0010](0010-pluggable-agents.md) | Bounded store outage and worker-local shutdown. Neither establishes controller restart recovery or general mixed-version upgrades. |
| [Capacity results](../dev/capacity-results.md) | Shared-runner worker/relay floors of 8 calls; loaded drain 44 ms in run 38147888686. No independent EC2 sizing. |
| [Guarantees: capacity and soak](../guarantees.md#capacity-and-soak) | Also records a later 50-call, one-hour passing soak on one 32-vCPU arm64 VM at `f779ec3`. That is shared-host evidence, not 50 calls per small EC2 worker. |
| [Soak results](../dev/soak-results.md) | Earlier hosted short-run evidence. Its pending full-soak statement predates the later result in the guarantee matrix. |
| [Cloud confirmation](../guarantees.md#cloud-confirmation), #48 | Separate VMs in one region; 25 planned moves, media resume p50/max 17/30 ms in one judged caller's three calls. This is cloud move evidence, not relay host replacement or automatic scaling evidence. |
| [Store limit](../guarantees.md#capacity-and-soak) | About 200 calls per Redis primary with 50% headroom is the tracked planning limit, not 500; it is not measured managed-store capacity. |

[Architecture](../architecture.md), [deploy](../deploy.md), [install](../install.md) and [guarantees](../guarantees.md) supply the
component and security contracts. Architecture's statement that Redis lacks TLS/ACL support is stale: `internal/processrun/run.go`
and deploy's managed-Redis section establish the current support. Do not propagate that sentence into the template.

## 1. AWS and a reviewable CloudFormation stack first

**Recommendation:** native CloudFormation YAML, with a region-specific quick-create link to an immutable public release template on
S3. Publish identical source and checksums with the release. AWS requires an S3 template URL for this console path [E1]. RunsOn
demonstrates customer-account installation and stack/parameter updates [E2]; it supplies no Relais availability or price evidence.

| Option | Experience, maintenance and audit tradeoff |
| --- | --- |
| AWS + CloudFormation | Console review without a local tool or external state service; inspect YAML/IAM and change sets. AWS-specific resources and public template distribution are maintenance costs. |
| AWS + Terraform/OpenTofu | Good existing-estate integration and readable plans; customer must install a tool and own state. Mocked-provider tests can run without credentials [E3]. Offer later as a wrapper, not a second independent implementation. |
| Pulumi or SST | Useful programmatic composition; adds a language/build/provider toolchain and state choices to the first install. No advantage here that warrants another customer prerequisite. |
| GCP or Azure first | Viable future adapters; selecting either now requires revalidating UDP ingress, fencing, shutdown and durable-store behavior. This investigation supplies AWS evidence, not a comparative cheapest-cloud finding. |
| Kubernetes/Helm | Fits existing clusters but makes customers supply cluster operations and UDP integration. Defer. |

Preview parameters: release/artifact digest, region/AZs, DNS/certificate, instance types, fixed worker count, capacity profile,
drain budgets, retention and secret references. No vendor access role. Create a dedicated VPC; defer existing-VPC support until
private API authentication. Public relay UDP and HTTPS signaling are the only ingress surfaces. Three static private worker IPs feed
production `-worker name=http://address` bootstrap; no dynamic registration or autoscaling is needed for preview. Provision and
price private-subnet egress for artifacts, AWS APIs and providers. Measure private RTT and jitter before applying ADR 0007's Linux
route tuning; do not extend it to public paths.

Updates replace the pinned template through a reviewed change set [E4]. Keep secrets, store namespace, encryption key and relay
address resources stable. Protect store/address resources from accidental replacement; deletion retains the store and secrets by
default and documents their continuing cost. CloudFormation rollback cannot reverse a newly written agent state format. A release
supplies a tested compatibility manifest and rollback boundary; reject an unsupported live upgrade before starting. Roll compatible
workers one at a time with surge capacity, then relay/control per [ADR 0009's
order](0009-planned-move-export-replay.md#compatibility-and-upgrade-order). Preview worker updates pre-drain through the running
control process before stopping each worker; re-bootstrap control when the fixed topology changes. Later ASG updates use the drain
coordinator. No TURN relay or TCP/TLS media fallback exists: callers whose networks block UDP fail (deploy guide).

CI on free GitHub-hosted runners: pinned `cfn-lint` schemas [E5], security-policy assertions, parameter branches, bootstrap/digest
checks, and cloud-API fakes for create/update/delete/rollback and duplicate lifecycle events. Real-process Linux tests exercise
drain, discovery and restart without cloud credentials. These checks cannot prove AWS provisioning, IAM enforcement, quotas or
cleanup; separate disposable, owner-approved runs must do that before publishing a working launch link.

**Main tradeoff:** simplest account-local entry point, with provider lock-in and release publishing work. **Change this if:**
customer adoption depends on an existing IaC standard, or AWS's minimum viable cost is unacceptable at the measured workload. A
second provider must implement the same readiness, admission, durable ownership, move-before-delete, fencing and evidence contracts;
add its own native template and lifecycle adapter, then repeat provider-specific fault/cost tests. Do not emulate AWS IAM.

## 2. Calls drive scaling; resource pressure guards admission

**Recommendation:** one fenced scaling reconciler changes ASG desired capacity for scale-out and coordinates a selected worker's
removal. Do not let two independent policies race to scale in. Use authoritative active calls **plus incoming reservations**, by
compatible agent/build pool. `Plane.pickExcluding` already balances leases plus reservations, but has no hard capacity ceiling
(`pkg/controlplane/controlplane.go`). Production dynamic registration is also missing: the current `-test-register-worker` and demo
registration are test surfaces (`cmd/relais-control/test_registration.go`). Hard admission limits precede the fixed-pool preview.
Authenticated dynamic registration precedes autoscaling only; the preview uses the existing production `-worker` bootstrap.

| Signal considered | Decision |
| --- | --- |
| Active calls | Primary for a homogeneous, benchmarked pool. Includes reservations, since they are promised work. |
| CPU alone | Secondary scale-out/admission guard. Media and external-I/O agents can be busy at low CPU; an average hides one overloaded worker. |
| Memory, packet rate, task queues | Per-worker guardrails and profile inputs; separate pools or weighted calls when work differs materially. Memory can rise after the scale decision. |
| Agent task count alone | Insufficient: provider latency and task cost differ; queued speech and media remain live between tasks. |

Export a new coherent pool sample: demand, ready/draining/pending counts, reservations, profile ID, sample age, resource pressure
and rejection counts. Reconcile locally every **5 s** and publish an account-local CloudWatch sample every **10 s**; both are
proposed settings, not measured delays. Existing `relais_worker_active_sessions` and `relais_control_active_calls` are checks, not a
complete scaling input (`pkg/mediaworker/metrics.go`, `pkg/controlplane/metrics.go`). Missing/stale samples freeze scale-in and
raise an alarm; they never become zero demand. CloudWatch is monitoring and audit, not an additional scale-in writer. Native target
tracking is considered [E6], but cannot by itself enforce relocation capacity or choose a safe worker. Keep its scale-in disabled if
added later.

Let `C` be the **validated operating call budget per worker for this exact profile**, `R` the validated relay budget, `N` ready
workers, and `D` calls plus reservations. Neither C nor R is a benchmark maximum. Also require `D + 1 <= S`, the validated store
budget; the tracked ~200-call planning limit is an initial ceiling to revalidate for the managed node. Store capacity can bind
before R. Use these proposed rules:

```text
u = 0.60                                      # proposed starting target
B = ceil(arrival_rate_bound * startup_p99)    # calls arriving before new capacity is ready
N_required = max(3, 1 + ceil((D + B + 1) / (u * C)))
admit only while D + 1 <= min(R, (N - 1) * C - 1)
```

B sizes future capacity; each admission checks the next call, so B is not reserved again forever. The extra worker covers one worker
failure; the extra slot covers a subsequent move, following [capacity.md](../dev/capacity.md). Applying the target to survivors
deliberately adds idle headroom. `startup_p99` includes signal age, reconcile delay, EC2 allocation/boot, artifact fetch, store
connect, registration and readiness. Measure it; no generic “VMs boot in X seconds” assumption. Until measured C/R/startup and an
arrival bound exist, preview uses a fixed pool and autoscaling stays disabled. Before then a public preview may admit only its
explicitly validated test load.

Scale out immediately when `N < N_required`, accounting for pending launches; size the increase to the forecast deficit, limited by
max workers. Do not count pending or merely EC2-running nodes as ready. Proposed guard triggers: worker CPU **70%** or memory
**75%** for **30 s**, or persistent task queue pressure, request more capacity and stop selecting that worker at its validated hard
limit. These percentages are design starting points, not Relais measurements. A provider-wide task backlog may need provider quota
handling rather than more VMs. At max workers or relay/store saturation, reject new calls cleanly and alert; more workers cannot
enlarge the single relay or store.

Scale in only after **10 min** below the proposed **40%** survivor-utilization threshold, with no resource alarm, recovery or
pending launch, and with the post-removal topology satisfying the same formula, including a further worker failure. Remove only one
worker at a time. Re-evaluate after empty confirmation and a full measured warmup interval; discard stale decisions. Keep scale-in
protection on all other instances. Proposed timings are policy knobs, to be replay-tested for flapping.

### Removal, lifecycle hooks and the real SIGTERM boundary

Before voluntary removal or worker rollout, reserve survivor capacity and durably record a unique operation/worker-generation ID.
Fence placement first. Prefer a **pre-drain through control while the source remains running**: a partial/uncertain drain retains
the source and retries; do not reduce ASG desired capacity yet. This avoids making the worker's bounded shutdown the only safety
net. Once local status, ownership inventory, reservations and transfer reconciliation all confirm empty, request targeted ASG
termination with desired-capacity decrement and remove protection only for it.

For health replacement and instance refresh too, the hook must **pre-drain before SIGTERM**. EventBridge delivers events through an
account-local queue; validate group/worker generation and action token, then mark/drain through control while the worker stays
running. Confirm empty from local status, authoritative ownership, reservations and transfer records. Only then let a narrow
supervisor send one SIGTERM to its owned service, verify exit and complete with `CONTINUE`. Persist operation state; reconcile
uncertain dispatch instead of blindly sending a second signal. Local lifecycle-state polling reconciles missing events. Heartbeat
during drain; on failed/uncertain drain, send `ABANDON` with **`TerminateHookAbandon: retain`**, and use default result `ABANDON`
[E23]. The retained worker stays alive, marked draining, and billed; it is excluded from desired capacity, so a replacement may also
be billed. Alarm on retained instances and retry idempotent drain; only an authorized cleanup operation terminates one after empty
verification [E23]. Cap retained-instance cost exposure, freeze repeated replacements and escalate failures rather than accumulate
machines.

The final SIGTERM still uses `pkg/workerapp/app.go` and `drain.go`: mark placement, optionally wait for handlers, then invoke
control drain while media, HTTP and heartbeats stay live. Defaults are `-drain-timeout=5s`, handler budget **2 s**, and no
`-agent-drain-move-reserve`. Keep the reserve unset until measured; control may spend **1 s** settling reservations. The
earlier-build local [quiescence sample](../dev/drain-quiescence.md) suggests **500 ms** only for its settled up-to-32-session
fixture, not raced reservations or a cloud default. Ordinary pre-drain uses urgent move export; SIGTERM quiescence is a shutdown
safety net, not permission to stop a nonempty cloud worker. Set service-manager stop timeout above the worker deadline and hook
timeout above dispatch plus drain/confirmation. Proposed hook heartbeat timeout: **120 s**, renewed during work.

**Hooks alone do not prevent eventual termination:** maximum wait is **48 h or 100 heartbeat intervals**, whichever is smaller [E7].
With retention and default `ABANDON`, failed/timed-out drains retain the instance [E23]. Retention cannot save a lost machine or
stop Spot reclamation; those bypass ASG protections [E24]. Start On-Demand. A direct SIGTERM outside this coordinator can still
close remaining calls at its deadline; document that limit. Existing crash/rollback limits remain.

Measure `decision_to_empty` from the durable removal decision, before mark/dispatch, until independent empty verification. Also
record decision-to-SIGTERM, last-adoption, full-drain, cloud termination and per-call decrypted audio/video gaps. Include failed,
timed-out and uncertain operations in the report. Use monotonic coordinator intervals and [distributed
evidence](../dev/distributed-evidence.md) for cross-host event/packet correlation; [takeover-budget.md](../dev/takeover-budget.md)
bounds evidence computation, not infrastructure takeover time. Report p50/p95/max with load, sample count and failures. **Current
cloud claim: NOT-RUN.** The 44 ms shared-runner drain brackets only its API interval. The customer never waits for natural call
completion; automation still takes measured time to move calls.

**Main tradeoff:** predictable safety/headroom costs idle VMs and a coordinator rather than a stock CPU policy. **Change this if:**
measured heterogeneous costs invalidate calls as a proxy, startup tails exhaust the burst reserve, or trace replay shows lower-cost
settings meet the same fault gates.

## 3. Relay availability in explicit phases

**Recommendation:** fixed Elastic IP plus a dedicated, movable secondary network interface (ENI). Keep its private UDP and HTTP
addresses stable too. EIP is regional/border-group scoped [E9]; an ENI moves only within its AZ and carries its IP attributes [E10].
This is a bounded first-cloud design, not an AZ-availability solution. Keep worker crypto state and caller's advertised candidate
unchanged.

| Option | Evidence and consequence |
| --- | --- |
| EIP alone | Preserves public candidate, but workers parse and retain a literal private relay address at startup (`pkg/workerapp/app.go`, `pkg/mediaworker/relayleg.go`). A public-IP move cannot repair that path. |
| EIP + secondary ENI | Preserves both paths within one AZ. Requires exclusive cloud host fencing, attachment sequencing, network configuration and a new cross-host relay recovery authority. Selected phase-B experiment. |
| EIP + secondary private IP | Lighter same-subnet/AZ alternative: reassign only the private service IP and reconcile its EIP mapping [E25]. Still needs host fencing, source routing, lease transition and both-leg tests. Compare attachment/mapping latency before choosing. |
| UDP NLB | UDP flow idle expiry is **120 s** [E11], but default unhealthy-target connection termination allows rerouting after health-check detection [E26]; it does not wait for idle expiry. Client IP is preserved [E12]. The successor can still receive RTP before STUN with no restored route. |
| Global Accelerator | Phase-C candidate, not live-flow failover: UDP idle timeout is **30 s**; established traffic stays on an unhealthy endpoint until idle/new flow [E27]. Continuous RTP can prevent failover. Custom routing has no health failover [E28]; fixed and transfer-premium charges apply [E29]. |
| Several relay candidates / ICE restart | Potential AZ recovery and regional choices. Needs multi-relay worker return routing, candidate/consent tests and client signaling changes. Current one-address behavior supplies no such guarantee. Defer. |

An NLB does not replicate authenticated routes, holds or rings. A relay replacement must restore current-owner routes before serving
and regain private worker registrations, preserving original consent deadlines (ADR 0003). Missing evidence waits for authenticated
STUN; ordinary media cannot invent consent. NLB health checks/deregistration do not prove ICE continuity, and node-specific
front-end addresses complicate the single-candidate contract. Do not call it transparent HA.

**Phase A — template preview and process deploy.** One relay host runs the ADR 0004 active and same-host standby under a supervisor.
Stage the compatible new binary as standby, stop active, observe changed instance/epoch and restored routes, then replenish standby.
This replaces a process without waiting for calls to end. Existing single-call loopback standby evidence has gaps below **1 s**
([guarantees](../guarantees.md#same-host-standby-relay)); loaded AWS process-deploy gap is **NOT-RUN**, not a subsecond promise.
Ring/hold loss remains. Host failure is an outage with no recovery bound in this phase; do not auto-reuse another host's same-host
lease.

**Phase B — automated same-AZ host replacement.** Maintain a warm replacement host. Serialize an operation: confirm the old instance
stopped/terminated through authoritative cloud state, move the dedicated ENI/EIP, configure source routing, authorize the new host
scope, restore routes and register workers. Verify both media legs. Never copy `-host-id` across machines, erase fencing
speculatively, or treat an unreachable predecessor as dead. A new cloud-fenced lease transition must preserve epoch history and
reject stale operations; ADR 0004 deliberately refuses foreign known hosts today. Planned host updates use the same sequence without
waiting for calls to end. With uncertain fencing or AWS API replies, retain the outage and reconcile; never activate a second
forwarder.

Expected gap is detection + cloud fencing + detach/attach + socket/route restore + worker registration, with consent recovery added
for missing routes. It may be seconds or longer; there is **no measured cloud bound** and no assertion that existing ICE consent
survives. Measure restart, frozen host, lost cloud replies, failback and mixed failures with long-lived browser and driver calls. If
fencing and attachment cannot finish inside the observed consent tolerance, **stop this path** and return an owner decision between
an NLB/multi-relay design and an explicit reconnect contract.

**Phase C — AZ survival, deferred.** ENIs cannot move across AZs. An EIP reassociation plus runtime private-leg rebinding, or
multiple relays behind tested ingress/candidates, needs a separate decision with phase-B evidence. Include Global Accelerator in
that comparison without treating health routing as established-flow recovery. No regional/AZ failure claim is included here.

Before README can say “forget the infrastructure,” pass paid, loaded create/update/rollback, worker scaling, controller replacement,
store failover and relay host-replacement runs with published gap/consent bounds, alerts and cleanup. Qualify the supported failure
domain as one AZ for the relay. An unqualified AZ-resilient claim also needs phase C. Do not edit README during this ADR. **Main
tradeoff:** simplest stable-address host experiment, but cloud fencing is slow/uncertain and one AZ remains a failure domain.
**Change this if:** phase B misses consent or availability objectives, or customers need AZ survival; then fund the
multi-relay/ingress design before a production claim.

## 4. One preview control process and a durable managed store

**Recommendation:** preview has one control process under a service manager, plus one signaling process. Fixed worker IPs use
production bootstrap (`cmd/relais-control/main.go`). ADR 0008 states that restart loses registry/history; `pkg/controlplane` also
keeps drain marks, reservations and uncertain recovery in memory. Automatic process restart is not reconstructed control state.
Established media should continue with live workers/relay/store and consent, but new calls, moves, end requests and worker crash
takeover need control. Measure restart during live calls in the preview; no unattended control replacement claim. Keep media
processes running during a control update; block new mutations, settle known operations, restart and report unavailable operations
honestly.

After preview, implement durable inventory, drain/operation records, safe recovery of uncertain exports/resumes and dynamic
registration on **one** control instance. Never blindly replay exports, creates or side effects. Prove old-process exit before
replacement; do not start two independent planes. Proposed later target: mutation readiness within **5 s** after verified
old-process exit, measured at validated load and during interrupted operations; this is a design target, not evidence. Add a warm
passive only if measured single-instance recovery misses it. That option needs authority generations checked at store/worker/relay
mutation boundaries, reconciliation before readiness, and pause/resume split-brain tests. Active-active remains rejected. Shared
Redis alone does not supply these guarantees.

Both durable-store options preserve confirmed writes across primary failover; use one shard with a primary and replica in different
AZs, TLS, ACLs and primary-only ownership reads [E13, E30]. Usual asynchronous Redis replication can lose confirmed ownership
decisions [E15]; backups and ADR 0008's **30 s** outage budget do not repair rollback. Never fall back to async or `default`.

| Option | Cost model, nodes and limits | Recommendation |
| --- | --- | --- |
| ElastiCache Valkey sync | `Durability: sync` exposed in CloudFormation [E14]. **18%** above standard node-hour price [E20]. Valkey **9.0+**, Cluster Mode Enabled, Multi-AZ and a replica required; at most **100 MiBps** write throughput per primary. Supported families are Graviton R8g/R7g/R6g/M8g/M7g/M6g/C8gn/C7gn; no Local Zones, Serverless, Global Datastores, Outposts or data tiering. Cannot enable durability on an existing non-durable cluster [E16]. | First evaluation on `cache.m7g.large`: predictable node-based premium suits this write-heavy workload. Not a proven cheapest choice. |
| MemoryDB for Valkey | Durable primary reads/writes across failover [E30]; Cluster mode is always enabled [E31]. Node-hours plus data-written GB above an allowance [E32]. Families include R7g/R6g/R6gd and burstable T4g [E33]; begin comparison on `db.r7g.large`, without data tiering/burst credits. Keep a replica in a second AZ; each primary supports up to **100 MB/s** [E34]. | Test the same protocol/fault gates if ElastiCache fails; select it if validated full costs/latency are better. No region or node-size equivalence is assumed. |

[Guarantees](../guarantees.md#capacity-and-soak) records roughly **85 KB** store request+reply per call-second in the short sample
and **97.3 KB** in the later soak. Pure conversion gives roughly **0.306–0.350 GB per call-hour** (decimal units), an arithmetic
estimate for total stream traffic, **not billable writes**. It includes reads/replies/protocol; MemoryDB bills written
key/value/command volume [E32]. Measure that volume and its share separately before comparing costs or the allowance. The pricing
page documents a **10 TB/month** allowance in Valkey examples but mixes **$0.20/GB** and **$0.04/GB** examples; the 2024
announcement says $0.04 above 10 TB [E32]. **Current applicable Valkey write rate/allowance scope: NOT-VERIFIED**. Obtain the dated
tariff before a cost-based choice. Compare `1.18 * sum(ElastiCache node-hours * rate)` against MemoryDB node-hours plus `max(0,
billable_write_GB - applicable_allowance_GB) * verified_write_rate`, with all other charges. This supports ElastiCache as the first
experiment by cost predictability, not as a cost winner.

Add Cluster discovery/TLS before creating the template: the process flags currently open standalone clients
(`internal/processrun/run.go`), though lower-level Cluster support exists. Test scripts, key slots, ACLs, scans, redirects and
reconnect against local disposable nodes, then managed failover. Require actual `EffectiveDurability=sync` for ElastiCache and
preserved acknowledged epochs/routes; measure checkpoint age and move/drain write tails on either store. If ElastiCache support or
latency fails, run the same tests on MemoryDB under a separately approved cap, then bring the owner the result. Do not recreate
ownership or weaken safety to accommodate a store. Durable-store client work stays before preview because durability cannot be
enabled later on a non-durable ElastiCache cluster [E16].

**Main tradeoff:** a smaller preview ships sooner, with explicit controller/security limits and a nontrivial database floor.
**Change this if:** either durable service fails compatibility/latency, measured costs favor MemoryDB, or the later restart target
justifies a passive control instance.

## 5. Publish allocated costs and measured headroom

**Recommendation:** start On-Demand `c7i.large` workers and relay, `m7i.large` control/signaling hosts, and the store above;
parameters allow larger sizes. These are **benchmark candidates**, not proven capacity or cheapest choices [E17–E18]. Keep at least
**3 ready workers**, subject to section 2. Avoid burstable CPU credits. Compare arm64 C7g/M7g with the same workload/fault tests;
the larger arm64 soak does not size a smaller instance. Agent binaries/dependencies must support the architecture.

Use `relais-load -capacity`, `-prices` and soak tooling with external callers/separate service hosts
([capacity](../dev/capacity.md)). Keep generator, survivor, drain-empty, decryption and loss gates. Publish commit, agent/build,
payload/bitrate, placement, buffers, TLS/durability, occupancy and boot tails. Report worker/relay/store CPU, memory, packet-rate,
traffic and N+1 spare capacity separately. Gate cloud capacity on ENA `pps_allowance_exceeded`, `conntrack_allowance_exceeded`,
`bw_in_allowance_exceeded` and `bw_out_allowance_exceeded` deltas; reject a passing-capacity claim if allowance drops occur or
required counters are unavailable. Record remaining conntrack headroom [E35].

```text
call_hours = integral(active_calls(t) dt) / 3600
allocated_cost_per_call_hour = (all_machine_cost + store_cost + network_cost
    + ingress_cost + disk_cost + observability/API_cost + secrets_cost) / call_hours
steady-state machine component = (W*Pw + L*Pr + K*Pc + S*Ps + Pstore) / T
public-transfer component = measured_public_B_per_call_second * 3600 / 2^30 * Ppublic_per_GiB
```

`W/L/K/S` count every running role, including idle workers; preview has no passive control or warm relay host. Later profiles add
them only when selected. Preview's proposed zero-call floor is:

| Role | Always-running machines/nodes | Preview arrangement |
| --- | ---: | --- |
| Workers | 3 | Separate EC2 hosts; fixed private IPs |
| Relay | 1 | EC2 host; active and same-host standby processes |
| Control | 1 | EC2 host; one process under a service manager |
| Signaling | 1 | EC2 host; test front end behind ALB/WAF |
| Store | 2 | Managed primary and replica in different AZs |
| Total | 8 | Six EC2 machines plus two managed store nodes |

Counts are design choices, not measurements. ALB/WAF, NAT or endpoints and other managed services also cost money and are outside
this machine count. A smaller evaluation profile is sensible: two workers, one relay host, shared control/signaling host and two
store nodes (**four EC2 + two store nodes**). It gives up a worker failure plus a further move when only one worker survives; it
also couples control and signaling failure/resource pressure. Use it for evaluation only, cap its load separately, and make no
preview N+1-plus-move claim. Never reduce durable store replication. `P*` are dated regional On-Demand inputs, not inferred CPU
prices; `T` must be a validated offered load with successful N+1 and drain. At low occupancy report actual integral allocation,
including zero-call idle cost separately. The capacity tool prices only workers/relay/control/store and public egress; retain its
report and add an explicit ledger for extra hosts, ALB, NAT or endpoints, EBS, IPv4, inter-AZ/region traffic, WAF, CloudWatch and
lifecycle/API/secret charges. Include the **18%** ElastiCache sync premium [E20], or verified MemoryDB node/write charges [E32], and
retained workers. No silent zero inputs; retained instances can exceed desired capacity and remain billed [E23]. Use capacity.md's
payload plus IPv4/UDP overhead for public bytes; avoid duplicate transfer charges. Exclude AI provider fees, tax and discounts
explicitly; list them separately for a full application estimate.

Inputs come from AWS EC2/transfer, ElastiCache and VPC pricing [E19–E21] plus the selected ingress, storage and monitoring tariffs
at measurement time. The fixed IPv4 dollar input used here is AWS's published **$0.005 per public IPv4 address-hour** [E21], not a
Relais measurement. No EC2 or managed store dollar rate is assumed, and **AWS cost/call-hour is NOT-RUN**. Existing tracked
estimates for another topology are not imported. Allocated cost at a demonstrated load floor is an upper-bound estimate for
unchanged workload/prices, not a cost minimum or a production capacity claim.

**Main tradeoff:** honest full-stack costs can look worse at small occupancy than marginal resource estimates. **Change this if:**
dedicated-host measurements justify smaller or arm64 hosts, a cheaper network layout meets the same isolation/fault gates, or real
agents become the main resource cost.

## 6. Preview isolation, then authenticated internal servers

**Recommendation:** preview creates a dedicated VPC with component security groups and no public private APIs. It trusts network
isolation; private HTTP is unauthenticated and carries key material (deploy guide). State this preview limit on the launch page. No
existing-VPC option until mutual TLS or equivalent authenticated private transport, required before general availability (GA).

| Surface / identity | Required boundary |
| --- | --- |
| Caller ingress | Public relay UDP and HTTPS on 443 only. `cmd/relais-signal/main.go` calls itself a bounded frontend for test callers: one shared bearer token, no code rate limiter. Preview puts it behind ALB plus AWS WAF rate-based rules [E36], blocks direct access, and exposes only its supported routes. Do not describe it as a production customer call API. |
| Private paths | Worker API/media, relay leg/admin, control HTTP and store accept only necessary component security-group paths; metrics stay private. No SSH ingress, peering or unrelated workloads in the preview VPC. Network isolation is the preview trust boundary, not mutual authentication. |
| Secrets | Secrets Manager holds snapshot key, shared signal token, Redis credentials and provider secrets. Fetch exact references into restricted runtime files/environment; never output values in templates, UserData, CLI arguments or logs. Secure dynamic references are unsupported in UserData [E22]. |
| Relay | Ownership/routes/relay-lease ACL, config and logs; no snapshot/provider keys. Validate Lua/scan command/key ACLs; code-level keylessness alone is not an ACL. |
| Worker/control/signal | Workers get only their pool's secrets; control needs the snapshot key. Signal gets its token, no store or snapshot key. Scope every role to its resources. |
| Later lifecycle coordinator | Approved group description/capacity/protection/targeted termination, queue consumption and hook heartbeat/completion only. Supervisor signals its owned service after empty proof; no arbitrary remote-shell permission. Validate retention behavior and IAM. |
| Later relay recovery | Separate role restricted to relay hosts and the designated ENI/IP/EIP operations; no worker networking or IAM grants. Persist operation-generation checks before mutations. |

Separate deploy/runtime roles; restrict `PassRole`. Constrain unavoidable wildcard reads/metric writes. Use IMDSv2, encrypted disks,
patched OS, verified pinned artifacts and compatible agent manifests. Disable demo/test registration and diagnostics; log outcomes,
not snapshots, SDP, secrets or media. Preview token rotation requires a coordinated restart and a published procedure. A
customer-facing call API with per-app credentials, independent rotation/revocation and authorization is **out of scope for preview**
and a separate GA gate; ALB/WAF does not supply application identity. Snapshot-key rotation needs its own compatible migration. An
authorized compromised worker remains trusted.

**Alternatives/tradeoff:** network isolation ships the bounded preview earlier, but cannot authenticate an authorized or compromised
peer. Public private APIs and broad admin roles remain rejected. **Change this if:** customers need shared networks sooner; move the
identity gate forward, not the boundary.

## Decisions for the owner

1. **Use AWS and a public launch link first.** Other clouds can follow.
2. **Keep at least three workers.** Aim for 60% use in normal operation. Set a hard cap. Keep automatic scaling off until we measure capacity.
3. **Ship a preview with one relay machine in one AWS data center area (Availability Zone).** If it fails, calls drop.
4. **Keep every confirmed call-ownership write.** Try ElastiCache with a setting that saves writes before confirming them. Test MemoryDB as the alternative. Compare full costs before the final choice.
5. **Ship the preview before adding a spare control process.** Start with one. Improve restart recovery next. Add a spare only if measured recovery is too slow.
6. **Count the full idle cost.** The preview keeps six machines and two database nodes running. Offer the smaller profile for evaluation with weaker failure protection.
7. **Keep only call media and the HTTPS front end on the internet.** Internal servers must prove their identity to each other before general release.
8. **Approve the direction only.** Each cloud test still needs separate approval and a cost cap.

Do you approve this direction and order, with no spend?

## Implementation plan

Use free GitHub-hosted runners for runnable Linux checks. Each paid run needs fresh owner approval, a priced resource/time envelope
and a USD cap before launch. Enforce cleanup deadlines, track all resources and verify teardown, including retained
workers/secrets/volumes/addresses/logs. Alerts alone are not a hard spending limit. No standing account is required for ordinary CI.
The heavier order was rejected because a fixed pool needs neither dynamic registration nor control handover. Network isolation
bounds the preview; durable-store client work cannot wait until later.

1. **Cluster/TLS store client and tests.** Blockers: owner store direction. Expose supported Cluster
   clients; demo scripts/key slots, TLS/ACLs, scans, redirects, reconnect and stale-epoch rejection on
   disposable nodes. Prepare equivalent ElastiCache/MemoryDB compatibility gates. **Free.**
2. **Hard per-worker cap, clean refusal and load telemetry.** Blockers: owner capacity direction.
   Use fixed production `-worker` bootstrap; no dynamic registration. Demo reservation races, per-worker
   limits and clean refusal; report actual load/headroom. **Free.**
3. **Preview template.** Blockers: 1, 2. Three static-IP workers, one service-managed control, one relay
   host with same-host standby, one signaling host, durable primary/replica, dedicated VPC/security
   groups, secrets, ALB/WAF, cfn-lint and bootstrap/update checks. Autoscaling off. Demo generated
   topology and local pre-drain/stop order; no mTLS or existing-VPC option yet. **Free.**
4. **Preview cloud proof and launch link.** Blocker: 3. **Paid; separate approval/cap per run.** Install,
   prove store failover retains acknowledged epochs, roll workers by pre-drain, deploy relay processes,
   restart control during live calls, and tear down. Measure gaps, unavailable operations, write tails,
   cost, a bounded preview load profile and cleanup. If ElastiCache fails, test MemoryDB under another
   approval/cap and return results.
   Publish the link only with observed limits; do not label lost controller state recovered.
5. **Single-instance control recovery, dynamic registration and lifecycle simulator.** Blocker: 4.
   Durable inventory/operation reconciliation; demo restart during create/move/drain/end, authenticated
   registration, duplicate/lost events, retained failed drains and SIGTERM only after empty proof.
   Measure restart readiness against the proposed 5 s target. **Free.**
6. **Scale-out measurement and move-before-delete with retention.** Blocker: 5. **Paid; separate
   approval/cap per run.** Measure C/R/S, boot tails, arrival bursts, ENA allowances and admission.
   Demo health replacement/refresh failures retained alive, retry/cleanup, max count, missing telemetry,
   compatible worker rollout and N+1 death; report decision-to-empty, caller gaps and retained cost.
7. **Internal identity and customer call API gates.** Blocker: 4. Add mTLS before GA or existing-VPC
   support; add per-app credentials and safe rotation as a separate API scope. Demo rejected peers
   and revoked credentials on runners. **Free; any cloud validation needs its own approval/cap.**
8. **Conditional warm passive control.** Blocker: 5 and measured failure of its 5 s target. Prove authority
   generations and uncertain-operation recovery locally before adding a spare. **Free first; each
   cloud handover/fault test is paid with separate approval/cap.** Skip if one-instance recovery suffices.
9. **Relay host replacement.** Blockers: 4, 5. Compare secondary ENI versus secondary-IP movement;
   prove cloud-fenced host transitions with fakes, then both legs, consent and failed cloud replies.
   **Free first; paid cloud runs each need approval/cap.** Stop the path if consent fails; return a
   measured NLB/multi-relay/Global Accelerator decision instead of promising continuity.
10. **Evidence, cost and claim gate.** Blockers: 6, 7, 9; 8 only if selected. Join dated prices, gaps,
    failure domains, alerts, rollback and cleanup. **Free from existing artifacts; extra cloud/soak runs
    each need approval/cap.** No “forget the infrastructure” claim before the supported domain passes.

## External evidence

All sources below were read **2026-10-17**. Provider documentation is capability evidence, not a measurement of Relais. Recheck
availability, pricing and schemas before implementation.

- **E1** [AWS quick-create links](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/cfn-console-create-stacks-quick-create-links.html).
- **E2** RunsOn [quickstart](https://runs-on.com/quickstart/) and [upgrades](https://runs-on.com/docs/maintenance/upgrades/).
- **E3** [Terraform provider mocking](https://developer.hashicorp.com/terraform/language/tests/mocking).
- **E4** [AWS change sets and parameter updates](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/using-cfn-updating-stacks-changesets-create.html).
- **E5** [AWS cfn-lint](https://github.com/aws-cloudformation/cfn-lint).
- **E6** [ASG target-tracking metric requirements](https://docs.aws.amazon.com/autoscaling/ec2/userguide/as-scaling-target-tracking.html).
- **E7** [ASG lifecycle hooks and bounds](https://docs.aws.amazon.com/autoscaling/ec2/userguide/lifecycle-hooks.html).
- **E8** [EC2 Spot interruption notices](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/spot-instance-termination-notices.html).
- **E9** [EC2 Elastic IP scope and reassociation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/elastic-ip-addresses-eip.html).
- **E10** [EC2 network-interface mobility](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/using-eni.html).
- **E11** [NLB UDP flow state](https://docs.aws.amazon.com/elasticloadbalancing/latest/network/network-load-balancers.html).
- **E12** [NLB target groups and client-IP preservation](https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-target-groups.html).
- **E13** Valkey [durability](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/durability.html) and [consistency](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Consistency.html).
- **E14** [CloudFormation replication-group durability property](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-elasticache-replicationgroup.html).
- **E15** [ElastiCache replication and data-loss limits](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Replication.html).
- **E16** Valkey durability [limitations](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Limitations.html) and [explicit configuration](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Configuring.html).
- **E17** [AWS C7i family](https://aws.amazon.com/ec2/instance-types/c7i/).
- **E18** [AWS M7i family](https://aws.amazon.com/ec2/instance-types/m7i/).
- **E19** [EC2 On-Demand and data-transfer pricing](https://aws.amazon.com/ec2/pricing/on-demand/).
- **E20** [ElastiCache pricing](https://aws.amazon.com/elasticache/pricing/).
- **E21** [VPC pricing, including public IPv4](https://aws.amazon.com/vpc/pricing/).
- **E22** [CloudFormation secure dynamic-reference limits](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/dynamic-references.html).
- **E23** ASG [retention policy](https://docs.aws.amazon.com/autoscaling/ec2/userguide/instance-lifecycle-policy.html), [configuration](https://docs.aws.amazon.com/autoscaling/ec2/userguide/configure-instance-retention.html) and [retained-instance management](https://docs.aws.amazon.com/autoscaling/ec2/userguide/manage-retained-instances.html).
- **E24** [ASG shutdown protection and interruption bypass](https://docs.aws.amazon.com/autoscaling/ec2/userguide/gracefully-handle-instance-termination.html).
- **E25** [EC2 secondary private-IP reassignment](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/using-instance-addressing.html).
- **E26** [NLB unhealthy-target connection termination](https://docs.aws.amazon.com/elasticloadbalancing/latest/network/edit-target-group-attributes.html).
- **E27** [Global Accelerator UDP idle and established-flow behavior](https://docs.aws.amazon.com/global-accelerator/latest/dg/introduction-how-it-works.html).
- **E28** [Global Accelerator custom-routing limits](https://docs.aws.amazon.com/global-accelerator/latest/dg/about-custom-routing-guidelines.html).
- **E29** [Global Accelerator fixed and transfer-premium pricing](https://aws.amazon.com/global-accelerator/pricing/).
- **E30** [MemoryDB confirmed-write consistency](https://docs.aws.amazon.com/memorydb/latest/devguide/consistency.html).
- **E31** [MemoryDB Cluster-mode requirement](https://docs.aws.amazon.com/memorydb/latest/devguide/managing-updates.html).
- **E32** [MemoryDB pricing](https://aws.amazon.com/memorydb/pricing/) and [2024 Valkey pricing announcement](https://aws.amazon.com/about-aws/whats-new/2024/10/amazon-memorydb-valkey/).
- **E33** [MemoryDB supported nodes](https://docs.aws.amazon.com/memorydb/latest/devguide/nodes.supportedtypes.html).
- **E34** [MemoryDB replication](https://docs.aws.amazon.com/memorydb/latest/devguide/replication.html).
- **E35** [EC2 ENA allowance counters](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/monitoring-network-performance-ena.html).
- **E36** AWS WAF [rate-based rules](https://docs.aws.amazon.com/waf/latest/developerguide/waf-rule-statement-type-rate-based.html) and [ALB application protection](https://docs.aws.amazon.com/waf/latest/developerguide/ddos-resiliency-example-tcp-udp.html).

- **E37** [AWS regions and Availability Zones](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/using-regions-availability-zones.html).

Comparable designs supply no Relais capacity inputs: LiveKit [distributed
deployments](https://docs.livekit.io/transport/self-hosting/distributed/) wait for participants to leave; its [agent
options](https://docs.livekit.io/agents/server/options/) separate job load from media placement.
[mediasoup](https://mediasoup.org/documentation/v3/scalability/) leaves distribution/signaling to the application. Relais chooses
moving calls; relay replacement still needs proof.
