# Live media architecture

Relais keeps a caller's WebRTC connection alive while its media worker changes.
The live path uses Pion v4 protocol libraries directly, so it can save and restore
connection state. It does not use the [legacy stored-frame pipeline](legacy/plugin-pipeline.md).
The earlier `relais-core`, `pkg/server`, and Pion v3 signaling path are retired.

## Components and flow

```text
App backend -- authenticated HTTPS --> relais-signal --> Control plane
                                                            |
Caller <== encrypted UDP ==> Relay <== private UDP ==> Media workers
                              |                             |
                              +---- Redis ownership --------+
                                   + encrypted checkpoints
```

- **Media worker (`pkg/mediaworker`):** terminates WebRTC, using ICE-lite
  (the caller drives network-path checks), DTLS (the encryption handshake),
  and SRTP/SRTCP (encrypted media and control packets). The accepted media is
  Opus audio and VP8 video. Audio runs through a per-call agent; video is echoed.
- **Relay (`pkg/relay`):** supplies the single public UDP address advertised in
  each answer. It reads STUN network checks, looks up the session owner, and
  forwards other encrypted packets without decoding them. A bounded flow table
  caches ownership. A caller route changes only after the owning worker
  authenticates a binding request. A versioned private-leg header carries the
  caller address between relay and worker.
- **Worker application (`pkg/workerapp`):** owns process configuration, startup, private HTTP, heartbeats, metrics, and bounded drain; `cmd/relais-worker` is its thin entrypoint.
  Custom v2 registrations use the same host; see [Build your agent](build-an-agent.md)
  and the keyless delayed-loopback example.
- **Control plane (`pkg/controlplane`):** places calls, moves them, drains workers
  (moves their calls away), and detects failed workers. A takeover assigns a
  crashed worker's call to an eligible survivor. Heartbeats also support explicit
  rejoin acknowledgement before a returning worker can serve calls again.
- **Session store (`pkg/sessionstore`):** records ownership leases and versioned,
  encrypted checkpoints. An increasing lease epoch fences out an old owner: it
  cannot keep writing as the current owner. Separate processes require Redis;
  in-process tests can use memory. Only workers and control receive the snapshot
  encryption key; the relay has ownership access without snapshot decryption.
- **Signaling (`cmd/relais-signal`):** wraps private control with a bounded,
  bearer-authenticated HTTPS API for controlled tests. It starts no media services.
  See [connect an app](connect-an-app.md).

## Moving and restoring a call

A successful planned move exports ICE credentials, DTLS connection state,
SRTP packet indexes, outbound track counters, and the accepted agent state with
its consumed-input position. The relay holds packets while ownership changes.
A drain applies that same move path to a worker's calls before stopping it.
The caller receives no second offer/answer exchange and keeps the same address.

After a worker crash, control fences the old owner and restores a safe checkpoint
on another worker. The relay buffers caller ciphertext, then replays selected
input through the new owner. A shared encrypted Redis frame cache also supports
video replay; a cache miss can request a live keyframe. The agent receives
checkpoint and snapshot ages plus possible duplicate-input ranges. Recent work
can repeat; applications must make external effects safe to repeat themselves.
See [agent continuity](guarantees.md#agent-continuity) and
[ADR 0006](adr/0006-agent-interface.md).

Session snapshots carry a format version. Workers must use compatible agent
factories and state versions. A general mixed-version upgrade is not guaranteed;
operators must preserve compatible readers before changing writers.

## Evidence and boundaries

The [guarantee matrix](guarantees.md) gives measured conditions, known failures,
and explicit non-guarantees. These include uncertain-reply rollback failures,
buffer overflow or expiry, and untested cross-host relay failover. A relay
restart can restore persisted routes at the same addresses; an optional standby
is limited to the same host. Control restart is tested locally; paid-cloud and
cross-host control failover are not established.

Private HTTP coordination is unauthenticated and can carry snapshot key material.
Restrict it to trusted hosts and users; loopback does not isolate local users.
DTLS-SRTP protects the caller-to-worker media leg. Redis supports verified TLS and ACL users in standalone and Cluster modes. See [deployment](deploy.md) for ports and security limits.

## Test and demo surfaces

[`pkg/callharness`](../pkg/callharness) supplies a Pion caller for in-process
and external topologies. [`cmd/relais-demo`](../cmd/relais-demo) runs the browser
cluster demo. The older [`cmd/echo-demo`](../cmd/echo-demo) shares one UDP socket
between in-process workers, or runs one relayed worker. See [demo guide](demo.md).

The historical plugin system has ingress, transform, and egress runners with
Redis or memory frame storage. Its camera is simulated and its WebRTC egress
polls storage. It is experimental, unmaintained, and separate from this media path.
