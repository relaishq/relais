# ADR 0013: Observe workers before losing calls with no takeover target

A short host pause can expire every worker heartbeat together. Three failed
target selections separated by 25 ms do not prove that a live worker's calls
cannot recover. Unrequested takeovers wait for worker observation within a fixed
deadline and prefer preserving the live source worker over moving its calls.
Planned-move recovery and control rebuild retain their existing retry and timing
rules; they do not use this target wait.

## Deadline and unchanged limits

```
D = min(firstDetection + 1.5 s,
        originalLastHeartbeat + 1.95 s,
        parentDeadline)
```

A missing heartbeat uses firstDetection as its anchor. The existing adoption
budget is 1.5 s. Default detection is 400 ms plus one 50 ms tick, totaling 1.95 s
against the existing 2 s hard-kill gate. Charging a longer observed host pause
against the same outage limit prevents another full recovery window. These are
work bounds, not guarantees under arbitrary scheduling delay. Retries and
returning heartbeats never renew D; transfer/resume share its remaining time.
An eligible first selection adds no wait. A failed source confirmation does not
let continuing heartbeats restart selection indefinitely. Only a prior rejection
for liveness needs another heartbeat check before issuing Transfer.

Wake waiters on eligibility changes, capacity release, or a dead source's first
returning heartbeat. Ordinary heartbeats do not wake all waiters. The configured
detector interval remains a fallback. Expiry is typed `ErrTargetWaitExpired`,
accounted once as `recovery_failed`; cancellation preserves pending work.
Unknown store outcomes retain existing authoritative reconciliation rules.

Waiting begins no relay hold and changes no consent deadline. The 3 s relay hold
backstop, 30 s consent limit, ADR 0004 relay fencing, ADR 0008 ownership retention,
and checkpoint-copy age, scaled margins, caller reserves and counter caps remain
unchanged. Longer ownership retention is not a cryptographic resume budget.

## One preservation decision per worker death cycle

The registry lock serializes two mutually exclusive decisions: preserve the
whole worker, or issue its first Transfer. No inventory or store I/O holds it.
The store's atomic Transfer is the ownership fence; relay old-leg fencing follows
before snapshot reads. Writing a control intent is not a fence.

Preservation requires fresh liveness, complete inventory matching every remaining
authoritative tenure, and a store re-check of each exact owner/epoch. It is
forbidden if any call's Transfer has been issued or remains uncertain, including
a completed transfer whose pending entry has already disappeared. Check again
before issuing Transfer. A heartbeat between the last liveness rejection and
that atomic decision triggers a new confirmation.

Preservation abandons all pending unrequested recoveries for that worker together.
The worker returns to service without a rejoin challenge, epoch/route changes,
or takeover/loss accounting. Drain and relay-registration gates remain intact.
Once any Transfer is issued, preservation is forbidden for the rest of the death
cycle: all other calls transfer or reach typed recovery loss, and the source
rejoins through the existing drop-all challenge. A worker is fully preserved or
follows fenced recovery.

Heartbeats received during recovery record liveness without granting eligibility.
The original heartbeat remains separate for deadlines, checkpoint reserves and
event timing. Missing or truncated inventory cannot prove safe preservation.

## Coverage and private diagnostics

The short control-plane tests simulate simultaneous heartbeat expiry. They cover
four no-target wakeups before a late target, preservation of two calls together,
a second call recovering after the first Transfer, heartbeat arrival at the
selection/Transfer boundary, ownership and inventory re-checks, bounded slow
confirmation, selective wakeups through Heartbeat, and planned/rebuild isolation.
These protocol-boundary tests do not establish real media or process timing.
The existing hard-kill completion/media-gap, consent and decryption assertions
remain unchanged.

The install driver retains bounded periodic private status/takeover samples,
a completion sample before assertions, and a final sample before teardown.
Capture failure records its error type and replaces older evidence. Artifacts
exclude secrets, snapshots and the topology state directory. Public signaling
sanitization remains unchanged.
