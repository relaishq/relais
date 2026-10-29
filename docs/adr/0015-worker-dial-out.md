# ADR 0015: Worker dial-out mode

Status: **Proposed** 2026-10-29. It stays Proposed until the owner reviews it.
Part of [#276](https://github.com/relaishq/relais/issues/276), first step of
the scale-to-zero map [#275](https://github.com/relaishq/relais/issues/275).
**Amends [ADR 0014](0014-python-agent-bridge.md)** (dated amendment there).
Code baseline: `cb64d96`.

A dial-out worker accepts no inbound connections. It opens every connection
itself: one command link to control, a media leg to the relay, and its existing
store and heartbeat connections. Control and the relay never dial it. This lets
a worker run behind NAT, on a function platform, or on a container service with
no public port. Inbound mode stays available, keeps its guarantees and stays
the default until dial-out has the same evidence.

## Owner summary

- **Commands.** After it registers, the worker opens one long-lived outbound
  stream to control. Control pushes commands over it. There is no polling.
- **Death.** A lost link alone never causes a takeover. The existing death rule
  decides, so takeover timing does not change.
- **Media.** The worker sends first to the relay's private leg. The relay learns
  the worker's address from that authenticated packet and sends media back to it.
- **Python bots.** A dial-out worker dials out to the bot runner and the bot.
  There is no rendezvous through relay or control for now.
- **No feature flag.** Dial-out is a worker connection mode, chosen in worker
  configuration. Inbound is the default. A later amendment can make dial-out
  the default after one release of equal evidence.
- **Mixed pools.** Calls move both ways between inbound and dial-out workers.
  The move state, the lease format and the store layout do not change.
- **Operators.** A dial-out worker needs zero inbound ports.
- **API freeze.** The open management API (#263 item 9) freezes only after this
  ADR is accepted.

## Today's inbound flow

| Connection | Direction | Purpose |
| --- | --- | --- |
| Control → worker private HTTP | Inbound to worker | `POST /sessions`, `DELETE /sessions/{id}`, `/export`, `/resume`, `/keyframe`, `/agent/control`, `GET /status` (`controlplane.RemoteWorker`) |
| Relay leg UDP → worker media UDP | Inbound to worker | Caller packets with the versioned caller-address header; drain barriers (`relayleg`) |
| Worker → control private HTTP | Outbound | Heartbeats every 100 ms with the worker's media address and rejoin token; mark-draining and drain on SIGTERM |
| Worker → store | Outbound | Lease renewals, fenced checkpoints, frame cache |
| Control → relay private HTTP | Not a worker connection | Worker allowlist (`POST /workers`), hold, move, release, replay, forget |
| Runner and bot → worker WebSocket (ADR 0014) | Inbound to worker | Bridge registry, bootstrap lookup, bot session; designed, not built |

Identity and fencing today:

- Control registers each worker by name and its **unique private relay-leg
  address** (`Plane.Register`). It learns the address from `GET /status`.
  Control has a fixed `-worker name=http://address` list.
- A session lease is `(session, worker address, epoch, expiry)`. The relay
  routes by the worker address in the lease. It accepts worker media only from
  that address.
- Worker observation decides death: no heartbeat for 400 ms, checked every
  50 ms. Recovery then takes each lease with an atomic store `Transfer` that
  installs a higher epoch. It does **not** wait for lease expiry. Lease expiry
  (34 s by default, ADR 0008) bounds store-outage retention; it is not the
  takeover trigger.
- A fenced old owner sees `ErrLeaseLost` on renewal or checkpoint and closes.
  The relay has already moved the flow, so its media cannot reach the caller.
  A returning dead worker must drop all old sessions and echo a rejoin token
  before it can serve again (ADR 0013 death cycle).

## Decision

### 1. One outbound command link

The worker is configured with control's URL, as today. In dial-out mode it
opens one long-lived HTTP/2 request to control's private listener:
`POST /private/workers/{name}/link`. The request body carries worker-to-control
frames. The response body carries control-to-worker frames. HTTP/2 comes from
the Go standard library: cleartext HTTP/2 on the trusted private network today,
TLS when internal mTLS (#263 item 10) lands. No new dependency is needed.

The link **tunnels the existing private worker API**. Each command frame holds
a request ID, a deadline, a method, a path and the same JSON body that the HTTP
API uses today. The worker serves it with its existing handler. Each reply
frame holds the request ID, a status and the same reply body. So create, end,
export, resume, keyframe, agent control and status keep their exact semantics
and payload formats. Control gets a link-backed `Worker` next to
`RemoteWorker`. The rest of control does not know which mode a worker uses.

Frames are length-prefixed. A large export or resume body is split into chunks
of at most 64 KiB. Small commands can go between chunks, so one 4 MiB move does
not block a hold-sensitive command for another call. The existing 4 MiB wire
cap and 2 s default command deadline stay unchanged.

The link does not redeliver commands. A command that was never written is a
definite **not sent** failure. A command written but not answered is an
**uncertain** outcome, exactly like a lost HTTP reply today. Planned-move
recovery and ADR 0009 rules handle it. No command runs twice because of a
reconnect.

### 2. Registration and the first hello

Control keeps its fixed worker list. A dial-out worker is listed without a URL:
`-worker name=dial-out`. Control accepts a link only for a listed name.
Dynamic discovery belongs to the management API (#263 item 9).

```text
worker                       control                         relay
  |-- link hello ------------->|                               |
  |   (name, instance, release,|                               |
  |    known death cycle,      |                               |
  |    session inventory)      |-- expect leg token T -------->|
  |<-- leg token T ------------|                               |
  |-- leg hello (T) from media socket ------------------------>|
  |<-- observed address A -------------------------------------|
  |-- leg address A ---------->|-- add worker (A, T) --------->|
  |                            |   relay checks T is bound to A|
  |                            |-- Register(name, A) (local)   |
  |-- heartbeats (A) as today ->                               |
```

The hello replaces control's first `GET /status`. It carries the same
inventory that status carries today. Control registers the worker with its
observed leg address `A`. `A` is the worker's identity for leases and relay
routes, as the private address is today. `Register` already rejects a
duplicate address.

### 3. Lost link versus dead worker

A lost link **never** marks a worker dead and **never** starts a takeover.

- Heartbeats stay separate outbound requests, as today. They do not ride the
  link. Death is still decided only by worker observation (400 ms without a
  heartbeat), followed by a fenced lease `Transfer` with a higher epoch.
  Takeover timing is therefore exactly today's.
- While the link is down, the worker is alive but **not eligible**. Control does
  not place new calls on it and does not choose it as a move or takeover
  target. Commands to it fail as not sent. Its existing calls keep their media,
  leases and checkpoints. A link that comes back is an eligibility change, so it
  wakes a target wait (ADR 0013).
- A planned move or drain away from the worker waits for the link, within the
  operation's existing deadline. It then fails with a typed retryable outcome.
  It never turns into a takeover.
- If the worker really dies, its heartbeats stop and today's takeover runs.
  It does not matter whether its link was up.

The worker reconnects at once, then backs off: 50 ms, doubling to a 1 s cap,
with full jitter. Control and worker send an HTTP/2 ping every 500 ms. Either
side treats the link as lost after 1.5 s with no frame. These timers only decide
when to reconnect. They never feed worker observation.

### 4. Reconnect and stale state

Every hello carries the worker instance ID (a random value per process start),
the last death cycle the worker knows, and its session inventory with the lease
epoch of each session. Control decides:

| Case | Control's action |
| --- | --- |
| Same instance, same death cycle, worker not declared dead | Accept. The new link replaces the old one, which control closes. Eligibility returns. Nothing else changes. |
| Control has declared the worker dead since that death cycle | Accept the link for the existing rejoin handshake only. The worker must drop and fence every old session and echo the rejoin token. No command for an old session is sent. |
| A session in the inventory has an epoch below the store's current epoch | That tenure is fenced. Control sends no command for it. The worker's own renewal or checkpoint already gets `ErrLeaseLost` and closes it. |
| A different instance with the same name while the old instance is still registered | Refuse, unless the old registration is dead and settled. The existing `replace` operation applies, as for an inbound worker restarted early. |
| An older link of the same instance arrives after a newer one | Refuse it. The newest accepted link is the only command path. |

Each command frame also carries the target session's epoch where the HTTP API
already has one (export, end, keyframe, agent control). The worker refuses a
command whose epoch does not match the epoch it holds, with a typed stale
outcome. The store `Transfer` stays the only ownership fence. The link never
grants ownership.

### 5. How the relay learns the route

The relay's public media socket and its private leg socket do not change. A
dial-out worker's media socket binds a local ephemeral port and sends first:

1. Control creates a random 128-bit leg token for this registration. It gives
   the token to the relay (`POST /workers/expect`) and to the worker (link).
2. The worker sends a **leg hello** datagram with the token from its media
   socket to the relay leg. The relay replies with the source address it
   observed, `A`. The reply comes from the same relay socket, so it passes
   through NAT, including symmetric NAT.
3. Control calls `POST /workers` with `A` and the token. The relay adds `A` only
   if that token is bound to `A`. Control stays the authority for the allowlist.
4. The worker adopts `A` as its worker address in place of its local socket
   address: in status, heartbeats and the leases it claims. From then on, relay
   routing, flow checks, holds, barriers and replay work as today. The worker
   accepts no session before it knows `A`; it is not registered before then.
5. The worker sends a **leg keepalive** every 1 s, with or without calls, to
   keep the NAT mapping open. Keepalives never feed death decisions.

The leg hello, its reply and the keepalive are new private leg frames, like the
drain barrier. They raise `relayleg.ContractVersion` and the manifest's relay
UDP contract. Inbound workers never see them.

Relay restart and same-host standby takeover keep the same addresses. Control
already restores the relay allowlist after a changed relay instance. The
continuing keepalives keep the NAT mapping open across the restart.

**NAT rebinding.** The relay never moves a route to a new source address on its
own: that would let a packet steal a route. If a keepalive with a valid token
arrives from a new address, the relay replies with the new address. The worker
reports it on the link. Control then drains the worker through planned moves.
The export travels over the link, which still works. After the drain, the worker
registers again with the new address. Calls on that worker see a media gap from
the rebinding until their move completes. Keepalives below the NAT timeout make
rebinding rare.

### 6. Python bot attach (amends ADR 0014)

In dial-out mode the worker dials out to the runner and to the bot. There is no
rendezvous through relay or control.

- The worker opens the registry WebSocket to each configured runner endpoint.
  The messages are unchanged; only the side that opens the socket changes.
- `assign` keeps its fields. The runner's reply adds the bot's private attach
  URL. The owner opens the session WebSocket to that URL and presents the attach
  token. The bot accepts only a connection whose token matches its pending
  assignment. The bot then sends `attach` as today, and the owner checks scope as
  today.
- The bootstrap lookup is not used for dial-out owners. The new owner reaches
  the runner over its own registry socket after a move or takeover.
- Token authority, revocation, generations, permits and recovery targets do not
  change.
- Runners serve both modes: they accept dial-out workers and keep the inbound
  path for inbound workers. A bot can move between an inbound owner and a
  dial-out owner. The bridge protocol version records dial-out support. The
  compatibility check refuses a dial-out worker with the bridge agent when the
  runner release lacks it.

### 7. Release compatibility for mixed pools

Mode is per worker. Both modes use the same private API bodies, snapshot,
component and export formats, Redis keys and lease format. A call therefore
moves inbound → dial-out and dial-out → inbound under the existing state-format
rules.

The [#252 release manifest](https://github.com/relaishq/relais/issues/252) adds:

- a `worker_link` contract (frame layout, hello fields, chunking) with its own
  golden fixture, like the private HTTP and relay UDP contracts;
- the raised relay UDP contract for leg hello and keepalive;
- a worker `dial-out` profile next to the default inbound profile.

Rules that preflight enforces:

1. **Upgrade order.** Upgrade relay and control first. A worker can start in
   dial-out mode only when control reads its `worker_link` version and the relay
   reads the new leg frames.
2. **Rollback.** Control or relay cannot roll back below the dial-out release
   while any dial-out worker is registered or holds a lease. Drain those workers,
   or restart them in inbound mode, first. A dial-out worker cannot roll back to
   a release without dial-out; change its mode first.
3. **Changing a worker's mode** is a worker replacement under a new name: drain,
   stop, add the new entry. This reuses the existing retirement rules.
4. **Inbound pairs stay valid.** A new control or relay with only inbound
   workers keeps every existing pair rule. The release pair against the latest
   published release must pass for inbound workers and for the new mode.

### 8. What changes for operators

| Process | Inbound mode | Dial-out mode |
| --- | --- | --- |
| Worker | Private HTTP and private media UDP reachable from control and relay | **No inbound port.** Egress to control HTTP, relay leg UDP, store and bridge runners. An optional loopback `-http` can still serve local metrics. |
| Relay | Public media UDP; private leg UDP; private HTTP | Same. The leg must be reachable from worker networks. |
| Control | Private HTTP | Same listener. It also receives command links. |

Worker flags: `-connect=inbound` (default) or `-connect=dial-out`. In dial-out
mode `-media` may be an ephemeral local address, and `-control` is required.
Control's `-worker name=dial-out` entry names the worker. Worker status and
metrics stay readable through control. The trust boundary of the private API is
unchanged: the link carries snapshot key material, so it stays on the trusted
private network until #263 item 10 adds mTLS.

Proposed defaults, each a documented parameter:

| Parameter | Default |
| --- | --- |
| Reconnect backoff | 50 ms start, doubling, 1 s cap, full jitter |
| Link ping; link lost after | 500 ms; 1.5 s without a frame |
| Leg keepalive | 1 s |
| Link chunk size | 64 KiB |
| Command deadline; wire cap | Existing 2 s; existing 4 MiB |

### 9. Management API freeze

Worker registration (the hello) and command delivery (the link) defined here
are inputs to the open management API (#263 item 9). The `worker_link`
contract starts at version 1 and stays unstable. Item 9 freezes only after this
ADR is accepted, so registration and command delivery do not change twice.

## Acceptance criteria

Evidence comes from the existing harness and `crash-run`, with a dial-out mode
added to each. All gates keep today's thresholds.

| # | Criterion | Target |
| --- | --- | --- |
| 1 | Echo calls on dial-out workers | Existing harness gates pass; zero decryption failures |
| 2 | Planned moves dial-out → dial-out, inbound → dial-out, dial-out → inbound | Caller gap within the existing pooled p95 ≤ 150 ms policy (baseline margin and clock uncertainty); at least 20 moves per direction, as the smoke run |
| 3 | Crash takeover of a SIGKILLed dial-out worker | < 2 s, the existing hard-kill gate; ten interleaved `crash-run` runs per mode |
| 4 | Drain, relay standby takeover and control restart with dial-out workers | Existing gates pass; zero decryption failures |
| 5 | Link dropped and restored while heartbeats continue: reset and black-hole, for 0.2 s, 1 s, 5 s and 30 s, idle and during calls | Zero takeovers, zero lost calls, zero decryption failures; worker eligible again after restore; a planned move after restore passes criterion 2 |
| 6 | A worker that is SIGKILLed while its link is down | Still taken over in < 2 s |
| 7 | Stale reconnect: a worker partitioned past takeover reconnects with an old death cycle and old epochs | No command for a stale tenure; worker completes the rejoin handshake; no stale media reaches the caller's raw socket (ADR 0008 method) |
| 8 | NAT rebinding test (address change under a valid token) | Relay keeps the old route; worker drains and registers again; calls survive |
| 9 | Release compatibility | Manifest covers `worker_link`, relay UDP contract and the dial-out profile; preflight refuses the unsafe pairs in section 7; the release pair against the latest published release passes |
| 10 | Inbound mode | Every existing inbound gate passes unchanged, with no threshold change |
| 11 | Bridge, once built | A bot keeps its call across moves and takeovers in a mixed pool, within ADR 0014 targets |

Commands that are in flight when the link drops follow the existing
uncertain-reply rules. They are reported separately from criterion 5 and do not
count as false takeovers. Criterion 5 drops the link only, not the heartbeats.

## Alternatives considered

- **Polling for commands.** The worker would ask control for work on a timer.
  Rejected. Each command would wait up to one poll interval, which eats the
  150 ms move budget. A short interval adds constant load from idle workers.
  The owner chose one pushed stream.
- **Rendezvous attach for bots.** Runner, bot and worker would meet through the
  relay or control. Rejected for now. It puts control or the relay in the bot's
  data path, adds a component to every attach, and needs a new trust design.
  The worker can already dial out to a runner it is configured with.
- **Heartbeats over the link.** One connection would carry both. Rejected. A
  link drop of more than 400 ms would then look like death and cause a false
  takeover.
- **A separate WebSocket library for the link.** Rejected. HTTP/2 in the
  standard library gives the same full-duplex stream with no new dependency.
- **Waiting for lease expiry to decide death.** Rejected. It would raise
  takeover time from under 2 s to the 34 s retention period.

## Consequences

- Control holds one stream per dial-out worker. The link becomes a component
  whose drops must be reported (`worker_link` reconnect count and down time in
  private status and metrics).
- Dial-out workers depend on reachable relay leg and control listeners. Moving a
  call to a new relay address (#278) needs a new leg hello to that relay.
- The glossary gains "command link" and "leg binding" when this ADR is accepted.
