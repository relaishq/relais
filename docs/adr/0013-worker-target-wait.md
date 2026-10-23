# ADR 0013: Observe workers before losing a call with no takeover target

Accepted for #230. A short host pause can expire every worker heartbeat together.
Three failed target selections separated by 25 ms do not prove that a live call
cannot recover. An unrequested takeover now waits for worker observation to
change, within a fixed deadline. It prefers the live source whenever that call
has not entered Transfer, even if a target also becomes eligible.

## Deadline and unchanged recovery limits

The target wait keeps one absolute deadline across retries:

```
D = min(firstDetection + 1.5 s,
        originalLastHeartbeat + 1.95 s,
        parentDeadline)
```

A missing heartbeat uses firstDetection as its anchor. The existing adoption
budget is 1.5 s. Default detection is 400 ms plus one 50 ms tick: together they
use 1.95 s of the existing 2 s hard-kill gate. Charging an unusually old heartbeat
against the same 1.95 s prevents a long observed pause from gaining another full
recovery window. These are work bounds, not a guarantee under arbitrary host
scheduling delay. Custom detection settings do not enlarge this wait.

Selection still runs immediately. First-selection success adds no wait. After
no target is found, observation changes wake selection; the configured detector
interval is a polling fallback. Transfer and resume share the time remaining
before D. Cancellation preserves unfinished recovery. Expiry produces typed
`ErrTargetWaitExpired`, accounted as `recovery_failed` once. Store errors and
uncertain Transfer candidates retain the existing reconciliation rules; a local
deadline cannot invent a negative store decision or prove an adoption failed.

Waiting begins no relay hold and changes no consent deadline. The relay's 3 s
hold backstop, 30 s consent limit, ADR 0004's independent relay lease/fencing
policy, and ADR 0008's 34 s default ownership retention remain unchanged.
Retention is not a cryptographic resume budget. Recovery still validates the
exact checkpoint copy age, scales margins outside the 550 ms envelope, reserves
caller counters through the remaining adoption deadline, and refuses unsafe
counter budgets. See `TestDefaultTakeoverSnapshotAgeBound`; its default 2.2344 s
age calculation assumes another full 1.5 s of adoption, not a universal cutoff.

## Source preference and fencing

The ownership fence is atomic `store.Transfer` in
`pkg/controlplane/move_recovery.go`, called after target selection in
`move_recovery_retry.go`. Relay old-leg fencing follows Transfer and precedes
snapshot reads. Writing a control intent is not this fence.

Worker observation records heartbeats during recovery but keeps the worker dead
and ineligible. The original pre-detection heartbeat remains separate for the
wait deadline, checkpoint outage reserve, and takeover event. Returning liveness
alone never grants placement, relay registration, resume retry or replacement.

Under the call lock, abandonment requires fresh source liveness, complete private
inventory with the exact installed call/owner/epoch, and unchanged authoritative
ownership. It changes neither epoch nor route and records neither loss nor a
successful takeover. Check source preference both before selection and before
issuing Transfer. The final liveness decision and Transfer-issued marker share
the registry lock; no registry lock spans inventory or store I/O. A heartbeat
racing selection triggers another confirmation before Transfer can be issued.

An issued Transfer, unresolved candidate, prior export, hold, route change or
resume attempt prohibits abandonment. A returning source cannot restore an old
fenced call. A definite interrupted Transfer is conservatively retried rather
than treated as permission to abandon.

For a source with both transferred and surviving calls, suppress the existing
blanket drop-all rejoin challenge. Leave that registration ineligible until
complete inventory matches its remaining authoritative tenures and no recovery
or incoming reservation remains. Superseded sessions self-fence on checkpoint
`ErrLeaseLost` (`pkg/mediaworker/checkpoint_snapshot.go`), as already covered by
`TestLeaseLostSnapshotSilentlyDiscardsSession`. Restore the cancelled death cycle
without dropping the surviving sessions; retain drain and relay-registration
gates. Workers whose recovery did not preserve a source call still require the
existing rejoin acknowledgement. Missing or truncated inventory cannot prove
safe source return. No worker-side API change is needed.

## Evidence and private diagnostics

`TestTargetWaitHostStall` expires all workers together and covers source return,
simultaneous source/target return, and late target eligibility. The target-wait
suite also covers fixed deadlines, cancellation, interrupted/uncertain Transfer,
mixed calls, incomplete inventory, immediate eligible-target recovery and
observation wakeups. `TestRecoveryHeartbeatRecordsLivenessWithoutRejoin` checks
both heartbeat protocols. These run in short mode. Mutation results and actual
commands belong in the handoff evidence; passing unit seams do not establish a
process timing result.

The install driver keeps bounded periodic private control status/takeover samples,
a completion sample before caller assertions, and a final sample before teardown.
Capture failure is best effort and records only its error type. Artifacts include
these JSON files, not secrets, snapshots or the topology state directory. The
public signaling API's explicit sanitization stays unchanged.

The existing `TestRelayHardKillTakeover` assertions remain: 20 kills, 2 s takeover
completion/media-gap bounds, and the full consent/decryption requirements. Its
process/media gate and the install driver are NOT-RUN for this implementation
phase; the orchestrator schedules them separately.
