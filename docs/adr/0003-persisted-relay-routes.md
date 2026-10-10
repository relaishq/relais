# ADR 0003: Restore authenticated relay routes before serving packets

## Context

A relay restart loses its confirmed routes. Until the next authenticated ICE
check, media is dropped and another session can claim an active caller's address
(ADR 0001, #20). Ticket #42 requires a restart gap under one second, without
reconnecting callers or exposing their media keys to the relay.

## Decision

- Persist only confirmed routing evidence: the caller address, session ID,
  lease epoch as route generation, confirmation time, last authenticated request
  arrival time, and consent deadline. Never persist a worker address in a route.
  The relay's store capability cannot read media snapshots or mutate leases.
  Redis route metadata, like lease metadata, is readable without a media key;
  media snapshots remain encrypted and unavailable to the relay.
- Use an **eager startup scan**, before starting either UDP packet loop. Each
  record must have a live lease with the same generation. Its worker comes from
  that lease. A stale record is ignored and deleted with a comparison against
  the original record. Startup fails closed on a store error, the startup
  deadline, or more records than `MaxFlows`; it does not serve a partly protected
  flow table. SCAN pages and total loaded records are bounded. Redis Cluster
  scans all masters. UDP source floods never initiate restore reads or scans.
- Preserve the original authenticated request time. Restoration, movement,
  ordinary media, unanswered checks and unmatched/replayed successes never
  renew consent. The restored address remains sticky for the remainder of its
  original window. Restored/persisted routes also stop forwarding when consent
  expires, even if unauthenticated media keeps arriving.
- Keep writes off the packet loops, using one bounded asynchronous writer.
  Updates coalesce by session and caller. Overflow and failed writes are
  counted in `RouteWritesDropped` and `RouteWritesFailed`. Write on confirmation,
  authenticated nomination, and trusted movement. Authenticated renewals write
  at most once per second per route. The writer reads the current lease and
  checks its owner against the local route before submitting its epoch-fenced
  write. Delayed writes cannot overwrite newer confirmation or renewal evidence.
- Redis stores records keyed by caller address within a per-session route hash.
  That hash shares `{sess:ID}` with the lease, so writes check the lease epoch
  atomically on standalone Redis and Redis Cluster. Matching release, expiry
  pruning and lease transfer delete that session's route hash. Memory implements
  the same lifecycle under its lease lock. Re-nomination removes the old caller
  address after the worker authenticates `USE-CANDIDATE`. A nomination watermark
  rejects delayed writes that would recreate an older address. Trusted movement
  republishes the route with its new epoch and unchanged confirmation/consent
  times. `ForgetSession` queues deletion even if the store still has a lease.
- Route retention is the consent deadline plus a one-second cleanup margin,
  independent of session/snapshot TTL. The margin never extends restoration or
  stickiness. Renewals extend the deadline only from authenticated request times.
- The control process detects a new relay instance through its private status
  endpoint and re-registers live workers' private legs. Dead or recovering
  workers are excluded. The caller and workers keep their original sockets and
  cryptographic state. This is registry recovery, not lease transfer or a worker
  rejoin acknowledgement.

## Consequences

Async writes are best effort: a restart before a confirmation or latest renewal
is stored can still wait for the next ICE check. Persistence can trail a renewal by the write cadence plus queue/store delay,
which shortens the restorable window. Overflow or store failures are observable and cannot stall established packet forwarding.
Large installations pay bounded startup scan and owner-read costs and must size
`MaxFlows` and `RouteRestoreTimeout`. The under-one-second claim is established
by the real-process loopback driver, not an unmeasured scale or cloud claim.

The existing address-only embedded relay API remains available with
`Config.Routes == nil`. The separate relay process enables persistence by default.
`-route-restore-off` keeps writes enabled and disables only restore, to measure
consent-check recovery against the same topology and caller.

`make crash-run CRASH_FLAGS="-relay-restart"` kills and reaps the old relay, starts
its replacement on the same public, private-leg and HTTP ports, and reports ten
runs each with restore on and off. Its caller-gap measurement uses actual
successfully decrypted packet arrivals across SIGKILL. The existing worker
crash/cache comparison remains the default driver mode. The driver caller's
consent-check cadence is two seconds; its restore-off gap depends on the restart
phase and does not establish the browser's broader two-to-five-second range.

Two concurrently live relays and standby fencing remain ticket #43.
