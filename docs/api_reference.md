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
  sequence), `control_invalid`, `control_unsupported` (legacy agent),
  `agent_capacity`, or an existing route/store/uncertain outcome.
- **400**: malformed JSON or refused query; **401**: existing bearer auth failure;
  **413**: frontend request bound; **502**: uncertain upstream transport failure.

The trusted control endpoint has the same call path. It resolves live store
ownership under the move/takeover lock and forwards to the owning worker's private
`POST /sessions/{id}/agent/control`. Never expose private worker/control APIs.
A planned move carries the exact accepted state and dedup window. A crash can
redeliver a 202-only message on the new owner with the same EventID. Checkpointed
old prefixes compact after four newer sequence positions; late duplicates then
return `control_stale`. Since sequences are contiguous and only checkpointed
prefixes compact, this proves durable acceptance for a retry of your own message:
stop retrying even though its detailed receipt has expired. Control receipts use
fixed-size digests and a separate measured sub-budget (at most one third of host
pending metadata; five unacknowledged records at default limits). A full budget
returns retryable `agent_capacity` before consuming the sequence and preserves
timer/task headroom. The decoded 1 KiB data also counts by its real encoded size.
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

## Plugin Development

Plugins must implement one of:
- IngressPlugin
- EgressPlugin
- TransformPlugin

See [the interfaces](../pkg/plugins/interface.go) and
[the legacy pipeline](legacy/plugin-pipeline.md). These plugins are not the
live audio-agent interface.