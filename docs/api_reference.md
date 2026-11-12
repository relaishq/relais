# Relais API Reference

> This page covers the lower-level media-worker and older echo-demo test APIs,
> plus the legacy plugin interfaces. To connect an application through the
> authenticated `relais-signal` frontend, use [Connect an app](connect-an-app.md).
> The production process worker uses a private API; this is not its public contract.

## Media Worker Signaling

The media worker (`pkg/mediaworker`) serves WHIP-style signaling; `cmd/echo-demo` mounts it next to the demo page.

```
POST /calls
Body: SDP offer (Content-Type: application/sdp)
201 Created with the SDP answer; Location: /calls/{id}

DELETE /calls/{id}
Hang up
```

See `SignalingHandler` in `pkg/mediaworker/signaling.go` for the current contract. Workers that share a UDP socket use `Socket.SignalingHandler` instead, which sends a hangup to whichever worker owns the call.

The echo demo adds two endpoints for its two workers:

```
POST /calls/{id}/move
Hand the live call over to the other worker; 200 with the move (timings, state size, held packets), 409 if it failed

GET /calls/{id}
The current owner, the move log and the owner's count of the caller's packets it could not decrypt
```

The control plane API (`/api/v1/sessions`, `/api/v1/plugins`) and the WebSocket signaling endpoint (`/ws/signaling`) were served by the retired `relais-core` server and no longer exist.

## Planned relay change signaling

Control and the authenticated `relais-signal` frontend support relay changes
for callers that opt in on `POST /calls` with the header
`Relais-Relay-Change: true`. The create body remains the full SDP offer with
`Content-Type: application/sdp`. This implementation uses a header rather than
ADR 0016's proposed create field `relay_change`, so existing SDP clients keep
their request format. Updating that ADR wording is a follow-up; the ADR is not
changed here. A single-relay deployment needs no opt-in or ICE restart.

After creation, an opted-in caller opens `GET /calls/{id}/events` using the same
bearer authorization as call deletion. It returns `text/event-stream` with
`relay-change` events whose JSON data contains `generation` and an absolute UTC
`deadline`. A reconnect receives the current pending event again; callers
should deduplicate by generation. Comment keepalives arrive every five seconds.
An `ended` event with a `reason` closes the stream on any terminal call outcome.

A capable caller also posts a full restart offer to `POST /calls/{id}/ice-restart`
without a `generation` query when ICE disconnects/fails, or inbound RTP/RTCP
stops for two seconds during live media. Control restarts the live current slot
first, then tries another live, untried slot. Retries share one journaled
15-second loss episode deadline; a slot is never retried within one episode.
This requires a configured relay set. Without one, the request returns 400
and does not start a recovery episode.

On `relay-change`, the caller creates an ICE restart offer on the same peer
connection and sends the full SDP to
`POST /calls/{id}/ice-restart?generation={generation}`, with the same bearer
authorization and `Content-Type: application/sdp`. A successful response is
`200` with the SDP answer. It changes ICE credentials and advertises the target
relay as the only candidate; DTLS and media keys continue. A stale generation
returns `409` (`relay_conflict`), and a changed media identity returns `409`
(`restart_mismatch`). The caller must process higher-generation recovery events
even after a failed offer. A planned change that cannot switch, or switches
without confirmation, rolls back to the old slot with a higher generation,
confirmed under the same mode.

### Relay change confirmation (ADR 0016 sections 5, 7, 10)

A switch commit alone never completes a relay change. A caller may also send
`Relais-Media-Progress: true` on `POST /calls` (it needs `Relais-Relay-Change`).
Control echoes each accepted capability as the same response header and fixes
the call's mode: `decode` (A) with both, `delivery` (C) with relay change only.
Without a configured relay set nothing is echoed and the call is ordinary; a
set of one slot accepts both capabilities and supports same-slot restarts. If
the selected worker lacks the C contract, a request for A is refused with `409`
`relay_unsupported`. A caller that requested A and sees only the relay-change
echo must delete the call and report `relay_unsupported`.

- **C (delivery).** The owning worker reads the caller's SRTCP receiver
  reports on the new path. Every required track must report a sequence sent
  after the switch. C completes a `delivery` call; it is diagnostic for `decode`.
- **A (decode).** After applying the answer and selecting the new pair, the
  caller discards 500 ms, then observes at least 500 ms of new decoded output
  on each required MID (non-concealed audio samples, new decoded video frames).
  It posts `POST /calls/{id}/media-progress` with `Content-Type:
  application/json` and `{"generation": 3, "decoded_mids": ["0", "1"]}`.
  Replies: `204` retained (or already confirmed); `409` stale, expired,
  unknown generation or a MID outside the required set; `400` malformed or a
  call without A; `410` ended; `413` over 4 KiB; `429` with `Retry-After: 1`
  above one request per second (burst 2); `503` retention uncertain or control
  not holding authority. A retired (aborted) or expired generation returns
  `409`. Only `204` is an acknowledgement.

Every successful `ice-restart` reply adds `Relais-Relay-Generation`,
`Relais-Relay-Deadline` (RFC 3339 UTC), `Relais-Required-Mids` (JSON array) and
`Relais-Relay-Confirmation` (`decode` or `delivery`). `relay-change` events add
`required_mids` and `confirmation`; the deadline is the planned or loss-episode
deadline and never grows. An unconfirmed planned change rolls back to the old
slot with a higher generation; if it is still unconfirmed at the 10 s planned
deadline, it becomes a 15 s stall episode. An episode that ends unconfirmed
sends `ended` with `reason: relay_change_failed` and `detail`
`delivered_decode_unconfirmed`, `delivery_unconfirmed` or
`confirmation_unsupported`, plus `generation`, `slot`, `confirmation` and
`missing_mids`.

## Fixed-pool admission and telemetry

Control `POST /calls` counts live store leases plus incoming reservations against
`-worker-call-cap` on each ready worker. Zero (the default) is unlimited.
Planned moves and drain destinations use the same budget. A move also holds a
separate source rollback slot until its synchronous coordination settles.
Only placement load includes that slot; it is not incoming work or demand. A lease
published before its outstanding reservation settles can count alongside that
reservation, so admission is conservative while operations are in flight.
A full destination
leaves the call on its source and returns typed `at_capacity`; a drain that
cannot reserve its destinations makes no moves. Crash takeover prefers the
least-loaded survivor and may exceed the cap to preserve live calls. Recovery
of an already-exported planned move uses the same overflow rule.

| Admission outcome | HTTP status | Body | Retry-After |
| --- | --- | --- | --- |
| Ready destinations are full | 503 | JSON `{"code":"at_capacity","message":...}` | `1` |
| No ready destination, including bootstrap waiting for the relay | 409 | Existing text `controlplane: no non-draining target worker` (`no_target`) | None |

Both are definite refusals before call creation and may be retried with bounded
backoff. Signal preserves the 503 `at_capacity` code and fixed `Retry-After: 1`,
and classifies it as retryable. Its existing generic response for `no_target`
is unchanged; the private typed code remains `no_target`.

Private control `GET /status` adds `ready` and one coherent `pool` observation:
`demand` (calls plus reservations), `worker_call_cap`, `ready_workers`,
`draining_workers`, `unreachable_workers`, `pending_workers`,
`never_answered_workers` (configured workers whose address is still unknown), `reservations`,
`refusals` (lifetime create counters by `at_capacity` / `no_target`),
`over_cap_calls` (live calls above each worker's cap), `sampled_at`,
`age_seconds`, and `stale`. Worker state counts are disjoint; pending includes
configured workers not yet registered and workers awaiting relay registration.
Initial readiness requires relay registration and at least one ready registered
worker. A failed relay poll preserves that readiness; an observed new relay
instance gates creates until its allowlist is restored;
a full ready pool remains ready and refuses new calls with `at_capacity`.

Control refreshes this observation during status requests and its ownership
reconciliation loop (at most 1.5 seconds between scheduled reconciliations).
Store reads run outside the registry lock; a registry/reservation change
invalidates the reads. All observation fields publish together under one lock. Refusal counters are
read live under that lock, independently of the cached observation.
Failed refreshes keep the previous timestamp and demand and mark it stale.
If ownership reads fail too, `/status` retains its error HTTP status and returns
JSON `ready: false`, the cached `pool`, and `error`; it does not claim empty calls.
An observation older than five seconds is stale. Before a complete successful
observation, `demand` is null, including when a configured worker's address is
still unknown. This is unavailable demand, not zero. Zero is valid only with a
complete successful observation. Consumers must check validity and freshness.

Private `/metrics` adds `relais_control_pool_demand`,
`relais_control_worker_call_cap`, `relais_control_pool_workers{state="ready|draining|unreachable|pending"}`,
`relais_control_pool_reservations`, `relais_control_refusals_total{reason="at_capacity|no_target"}`,
`relais_control_pool_over_cap_calls`, `relais_control_pool_never_answered_workers`,
`relais_control_pool_sample_timestamp_seconds`,
`relais_control_pool_sample_age_seconds`, `relais_control_pool_sample_valid`, and
`relais_control_pool_sample_stale`, alongside `relais_control_active_calls`.
Before the first observation, all sample-derived metrics (demand, workers,
reservations, over-cap calls, never-answered workers, timestamp and age) are
omitted. Configuration and live refusal counters remain available.
Missing demand omits the demand metric and sets valid=0, stale=1. Cached demand
retains its value with stale=1 on failure or age expiry; scrapes do not relabel an
old observation with the current timestamp.

## Worker read-loop metric

A worker's private `/metrics` serves `relais_worker_*` counters and gauges, such
as `relais_worker_packets_in_total` and `relais_worker_active_sessions`
(`pkg/mediaworker/metrics.go`). They have no session labels.

`relais_worker_read_burst_drain_seconds` is a histogram with no labels. It is
a heuristic: each observation is the approximate length of one busy period of
the worker's single UDP read loop (issue #139). A period runs from reading its
first packet to finishing its last, while no wait for the next packet is longer
than 100 µs. A lone packet is a period of its own.

- A period is not packet waiting time. It includes the short waits between
  packets. For example, packets 90 µs apart, each finished in 10 µs, form one
  long period although nothing queues.
- A stall longer than 100 µs between packets ends a period, even a stall in
  scheduling or inside the read system call. A long burst can then appear as
  several shorter periods.
- A period is recorded when the next packet arrives, or when the read loop
  stops.

Long periods suggest that bursts kept the loop busy. For example, watch
`histogram_quantile(0.99, rate(relais_worker_read_burst_drain_seconds_bucket[5m]))`.
The ratio
`rate(relais_worker_read_burst_drain_seconds_sum[5m]) / rate(relais_worker_packets_in_total[5m])`
is the elapsed busy-period time per counted packet. It is not the processing
cost per packet: it includes the short waits, and the packet counter excludes
a few packets the loop reads and drops.
The metric costs two clock reads per packet and one histogram update per busy
period. It allocates nothing and takes no lock. `BenchmarkReadLoopBurst` in
`pkg/mediaworker` is the matching burst fixture.

## Private worker placement fence

```text
POST /private/workers/{name}/mark-draining
204 No Content (including repeated marks)
404 JSON {code:"not_found",message:...} for an unknown registration
```

This control-process endpoint marks only the named registration draining.
Placement excludes it and `/status` reports `draining`; heartbeats do not clear
it. It moves no calls, waits for no incoming reservations, and acquires no call
lock or drain slot. Authentication is the same as private worker heartbeats:
**none beyond the existing private listener/network restrictions**. Do not expose
it through the public signal frontend. It grants no new public control authority.
`POST /workers/{name}/drain` shares the mark and then runs existing planned moves.

After the worker acknowledges the mark, it refuses raced `POST /sessions`
creates with HTTP **409** and typed `worker_draining`. This private refusal is
definite only after any unregistered claim has been released: no session, call
or lease remains. Failed claim cleanup returns an uncertain outcome and is not
retried. Control retries a definite refusal on another
non-draining worker, excluding every worker that refused. Retries are bounded by
the number of registered workers at the start of placement. An initially pinned
create can use another worker after this refusal. Control never retries a lost
reply, timeout, cancellation or unknown create failure.

Public create responses stay unchanged. If no worker is left, control returns
**409**, `Content-Type: text/plain; charset=utf-8`, with the existing body
`controlplane: no non-draining target worker` followed by a newline. The signal
frontend keeps its existing sanitized text response. No placement-specific JSON
or `Retry-After` mapping is added. `worker_draining` is placement-only and cannot
be returned by the Control route. See [worker shutdown](deploy.md#worker-shutdown).

## Routed agent Control

The authenticated `relais-signal` frontend accepts:

```text
POST /calls/{id}/agent/control
Content-Type: application/json
Authorization: Bearer <existing deployment token>
{"id":"press-1","sequence":1,"name":"ptt.start","data":"AP8="}
```

`data` is optional base64 binary data, at most 1 KiB decoded. IDs and names are
nonempty UTF-8, at most 128 bytes. New messages use contiguous per-call sequences
starting at 1 and unique IDs. Retries preserve the whole message across moves,
takeovers and lost responses. No query parameters or private target are accepted.

- **202**: locally accepted callback/state/receipt; crash loss is possible.
- **200**: an acknowledged checkpoint records acceptance.
- Both return `{id,sequence,event_id,checkpointed}`; only `checkpointed` changes
  when the same retained receipt becomes durable.
- **409**, with a typed JSON `code`: `control_conflict` (retained ID/sequence
  collision), `control_stale` (durably accepted and compacted sequence; stop retrying your original message), `control_order` (future
  sequence), `control_invalid`, `control_unsupported` (legacy agent or insufficient registration limits),
  `agent_capacity`, or an existing route/store/uncertain outcome.
- **400**: malformed JSON or refused query; **401**: existing bearer auth failure;
  **413**: frontend request bound; **502**: uncertain upstream transport failure.

The trusted control endpoint has the same call path. It resolves live store
ownership under the move/takeover lock and forwards to the owning worker's private
`POST /sessions/{id}/agent/control`. Never expose private worker/control APIs.
A planned move carries the exact accepted state and dedup window. A crash can
redeliver a 202-only message on the new owner with the same EventID. Checkpointed
old prefixes compact after one newer sequence position; late duplicates then
return `control_stale`. Since sequences are contiguous and only checkpointed
prefixes compact, this proves durable acceptance for a retry of your own message:
stop retrying even though its detailed receipt has expired. Control receipts use
fixed-size digests and a separate measured sub-budget (at most one third of host
pending metadata; two unacknowledged records at default limits). A full budget
returns retryable `agent_capacity` before consuming the sequence and preserves
timer/task refusal headroom. Only actual held receipt bytes count toward other
event admission; Control-bearing snapshots use compact versioned task refusals.
Insufficient registration limits return definite `control_unsupported` instead of
retryable capacity. The decoded 1 KiB data also counts by its real encoded size.
Before Started is accepted, retryable `control_not_ready` refuses admission.
After quarantine and successful Restore, a Control Handle error/deadline outside
Save returns retryable `control_rejected`, consumes no sequence and keeps the
call alive. Save/Restore and invalid-effect failures remain terminal.
Move/Drain wait a bounded time for Control with operator priority; each Control
route hold is capped at 500 ms and takeover may cancel it. Routing/transport
uncertainty still requires an identical retry. Malformed JSON, unknown fields,
invalid base64 and wrong types return definite 400 `control_invalid`.
See [the agent guide](build-an-agent.md#routed-control) for pending byte accounting,
retry rules, and the unchanged deployment/authentication limits.


Control error classes (codes preserve the existing HTTP status):

| Class | Codes | Client action |
| --- | --- | --- |
| retryable | `worker_link_unavailable`, `worker_command_not_sent`, `stale_tenure`, `agent_backpressure`, `agent_capacity`, `barrier_timeout`, `canceled`, `checkpoint_clock`, `closed`, `control_not_ready`, `control_rejected`, `deadline`, `export_not_flushed`, `held`, `hold_expired`, `hold_limit`, `lease_lost`, `move_in_progress`, `no_target`, `at_capacity`, `not_established`, `replay_in_progress`, `replay_not_ready`, `replay_target_changed`, `session_exists`, `stale_move`, `transient`, `unknown_session` | Retry the identical message with bounded backoff and a deadline. |
| definite | `relay_switched`, `relay_change_rolled_back`, `restart_mismatch`, `relay_unsupported`, `relay_conflict`, `move_budget`, `agent_incompatible`, `agent_oversize`, `agent_restore`, `agent_save`, `agent_version`, `buffer_disabled`, `bridge_revoked`, `control_conflict`, `control_invalid`, `control_order`, `control_stale`, `control_unsupported`, `ended`, `export_incompatible`, `not_found`, `sequence_budget`, `srtcp_exhausted`, `dtls_exhausted`, `unknown_call`, `unsupported_offer` | Stop automatic retries; resolve ordering, registration, limits or call state. For control_stale, acceptance is durable. bridge_revoked requires a replacement generation; pending activation uses retryable export_not_flushed. |
| uncertain | `internal`, `uncertain` | Acceptance is unknown; retry the identical message. Internal is exposed as uncertain. |

`stale_move` means the relay refused a call route move because a newer move
already applied to the call; the call stays where the newer move put it. Route
moves are ordered only by a non-zero lease epoch. A move from a released
control plane carries epoch zero and is applied unordered.

`control_order` is retryable while an earlier sequence from this controller is
unconfirmed (neither a checkpointed 200 nor `control_stale`). A takeover can
lose a 202-only start/stop pair and receive the stop retry first. Retry both
identical messages until the gap closes. With no earlier unconfirmed message,
`control_order` is definite. After a 202, allow at least the 30 s outage budget
plus an acknowledgement margin (the demo allows 45 s). If all responses were
nonaccepting `agent_capacity`, `control_not_ready`, `control_rejected`, or
`agent_backpressure` refusals, the demo reports "not accepted" and reuses the
sequence. Any uncertain response prevents that reuse.


`move_budget` is definite for Control: exceeding move-memory admission fails
closed under the existing host contract. `agent_incompatible` is definite because the registration, state or callback effects need a software
or operator fix; replaying the same message does not repair them. `closed` means
retry on the live route. Unknown/missing codes are uncertain on every status,
including 409. Only a non-envelope 400 (private JSON decoding failure) becomes
`control_invalid`; an unfamiliar JSON error envelope remains uncertain.

## Plugin Development

Plugins must implement one of:
- IngressPlugin
- EgressPlugin
- TransformPlugin

See [the interfaces](../pkg/plugins/interface.go) and
[the legacy pipeline](legacy/plugin-pipeline.md). These plugins are not the
live audio-agent interface.
