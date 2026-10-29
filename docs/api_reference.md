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
| retryable | `worker_link_unavailable`, `worker_command_not_sent`, `stale_tenure`, `agent_backpressure`, `agent_capacity`, `barrier_timeout`, `canceled`, `checkpoint_clock`, `closed`, `control_not_ready`, `control_rejected`, `deadline`, `export_not_flushed`, `held`, `hold_expired`, `hold_limit`, `lease_lost`, `move_in_progress`, `no_target`, `at_capacity`, `not_established`, `replay_in_progress`, `replay_not_ready`, `replay_target_changed`, `session_exists`, `transient`, `unknown_session` | Retry the identical message with bounded backoff and a deadline. |
| definite | `move_budget`, `agent_incompatible`, `agent_oversize`, `agent_restore`, `agent_save`, `agent_version`, `buffer_disabled`, `control_conflict`, `control_invalid`, `control_order`, `control_stale`, `control_unsupported`, `ended`, `export_incompatible`, `not_found`, `sequence_budget`, `srtcp_exhausted`, `dtls_exhausted`, `unknown_call`, `unsupported_offer` | Stop automatic retries; resolve ordering, registration, limits or call state. For control_stale, acceptance is durable. |
| uncertain | `internal`, `uncertain` | Acceptance is unknown; retry the identical message. Internal is exposed as uncertain. |

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
