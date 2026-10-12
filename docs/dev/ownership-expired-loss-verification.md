# Ticket 99 verification

Checkout: `fix/99-ownership-expired-loss`, original HEAD `9665140744d021ca1199a5fa660ddaaa7361b211` (main `05104a0` plus the #95 driver). Changes are uncommitted. Host: `MacBook-Pro.local`; Go `go1.26.9 darwin/arm64`. Read-only issue 99/100/95 access succeeded. No GitHub writes or machine-wide installs.

## Design

The control plane retains each successful placement's owner/epoch. Built-in workers return the token in the creation acknowledgement, so control does not need a successful subsequent store read to retain it. Registry reconciliation runs independently of heartbeat death and status polling. Only authoritative missing ownership is loss; transient reads retain the record. The per-call lock, terminal guard, and immutable lost token prevent double accounting and stale cleanup. Missing ownership never resumes.

Live worker fencing remains unchanged. It does not report a second loss. Control records `ownership_expired`, one bounded status event, and the labelled process-local loss counter. Cleanup errors retain a terminal job for retry without hiding or recounting the loss. Store release compares owner/epoch atomically. Conditional relay cleanup preserves newer live ownership and cached lookup/move/restore epochs, including address reuse. It never queues an unconditional persisted-route forget for this path.

## Red and green

The three new regression files were copied into a throwaway `git archive` of base `9665140`. The production files were unchanged in that archive. All 11 top-level regression tests failed on base with behavioral failures (zero loss, forgotten successor, missing endpoint or missing terminal accounting), not compilation failures. The corresponding current regressions pass. Placement and cleanup retry also have separate red/green records. The soak cause-label regression failed before its consumer fix and passes after it.

```sh
RELAIS_TEST_REDIS_ADDR=127.0.0.1:16499 RELAIS_TEST_REDIS_REQUIRE=1 go test ./pkg/controlplane ./pkg/relay ./pkg/callharness -run 'TestOwnershipExpired|TestTenureCleanup|TestRelayOwnershipExpiredLoss' -count=1
```

## Driver results

Every row is a real-process loopback trial with ten seconds of post-fault observation. There are zero renegotiations in every row. Moderate used scaled margins and one envelope event. Long/live have no retained checkpoint age, zero resume margins, zero envelope events, one counted loss, no snapshot/lease, and no cached/persisted route. The live workers stay alive with zero sessions and zero worker-death detections. Both the store-stall report and caller report retain the loss cause.

| Variant | Run | Outcome | Losses | Cause | Decryption failures | Reconnects |
|---|---:|---|---:|---|---:|---:|
| moderate | 1 | resume | 0 | - | 0 | 0 |
| moderate | 2 | resume | 0 | - | 0 | 0 |
| moderate | 3 | resume | 0 | - | 0 | 0 |
| long | 1 | definitive-loss | 1 | ownership_expired | 0 | 0 |
| long | 2 | definitive-loss | 1 | ownership_expired | 0 | 0 |
| long | 3 | definitive-loss | 1 | ownership_expired | 0 | 0 |
| live | 1 | definitive-loss | 1 | ownership_expired | 0 | 0 |
| live | 2 | definitive-loss | 1 | ownership_expired | 0 | 0 |
| live | 3 | definitive-loss | 1 | ownership_expired | 0 | 0 |

```sh
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=moderate -runs=3 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=long -runs=3 -after=10s
env -u RELAIS_REDIS_ADDR ./bin/crash-run -store-stall=live -store-stall-duration=5s -runs=3 -after=10s
```

Full trial artifacts:
- moderate: `bin/crash-run-1872510356`
- long: `bin/crash-run-997867925`
- live: `bin/crash-run-3594829`

## Required checks

- `go vet ./...`: PASS.
- `make build`: PASS.
- `go test -race -count=1 ./pkg/controlplane ./pkg/mediaworker ./pkg/relay ./pkg/sessionstore ./pkg/metrics ./cmd/crash-run ./cmd/relais-load` with owned Redis: all packages PASS. The final control-plane rerun passes after preserving the waiting-End `ErrNotFound` contract; the earlier failure and corrected run are both retained. Final corrected command: `go test -race -count=1 ./pkg/controlplane`.
- `make test-harness-short`: final serial run PASS (511.754 s).
- `go test -race -count=1 ./pkg/callharness -run '^TestRelayOwnershipExpiredLoss$'` with owned Redis: memory and Redis PASS.
- `git diff --check`: PASS.
- `actionlint`: N/A; no workflow changed.

One earlier broad harness run failed `TestAgentSlowQueueMoveExactContinuation/memory`: expected output 46, observed 47. Its assertions were unchanged. Ten focused race runs passed on base and ten on current, followed by a passing final full serial suite. The failure's cause is unproven; do not present it as a confirmed pre-existing or environment-only failure.

Logs are retained under `bin/99-verification/`. This directory is ignored and contains verification artifacts, not source changes.

## Changed files

| File | Change |
|---|---|
| `cmd/crash-run/store_stall.go` | Judge definitive ownership loss from status, cause-labelled metrics, cleanup, and caller reports; retain checkpoint judging for moderate stalls. |
| `cmd/crash-run/store_stall_test.go` | Reject missing/double accounting, wrong causes, stale routes, resumed media, and caller integrity failures. |
| `cmd/relais-load/soak.go` | Keep soak loss detection effective after adding cause labels. |
| `cmd/relais-load/soak_test.go` | Regression coverage for labelled ownership and checkpoint losses. |
| `docs/adr/0002-checkpoint-age-envelope.md` | Record retained inventory, placed-lease acknowledgement, one loss, independent cleanup retry, and restart limitation. |
| `docs/adr/0003-persisted-relay-routes.md` | Record tenure-conditional cache cleanup and fenced persisted-route lifecycle. |
| `docs/dev/store-stall.md` | Document the new long/live acceptance and unavailable checkpoint age. |
| `pkg/callharness/handover.go` | Carry lost/cause fields in caller move reports. |
| `pkg/callharness/ownership_expired_test.go` | Exercise crashed and live ownership expiry through real callers on memory and Redis. |
| `pkg/callharness/takeover.go` | Copy terminal loss metadata and use the event start when no heartbeat/death observation exists. |
| `pkg/controlplane/controlplane.go` | Retain placed leases and terminal cleanup jobs; reconcile status/End; forbid terminal moves. |
| `pkg/controlplane/metrics.go` | Expose bounded loss causes and exclude terminal cleanup jobs from active calls. |
| `pkg/controlplane/metrics_integration_test.go` | Validate cause labels and assert every zero-valued loss series exists. |
| `pkg/controlplane/metrics_test.go` | Assert aggregate losses across labelled series. |
| `pkg/controlplane/ownership.go` | Retain ownership, reconcile authoritative absence, count once, and retry conditional cleanup using an immutable lost token. |
| `pkg/controlplane/ownership_expired_test.go` | Cover concurrent status, detector-only accounting, racing successor, hangup/fence races, placement read failure, and cleanup retry. |
| `pkg/controlplane/remote.go` | Carry placed leases and call the conditional relay cleanup API. |
| `pkg/controlplane/takeover.go` | Drive registry reconciliation, preserve routed tenure, guard terminal calls, and label existing losses. |
| `pkg/controlplane/takeover_rework_test.go` | Adapt the existing loss metric assertion to labelled series. |
| `pkg/mediaworker/http.go` | Include the placed lease in the private creation acknowledgement. |
| `pkg/mediaworker/worker.go` | Capture the placed lease before publishing the successful creation reply; fencing/TTLs unchanged. |
| `pkg/metrics/process.go` | Allow bounded constant labels on process-local samples. |
| `pkg/relay/flows.go` | Track lookup/move/restore epochs and remove only the matching cached tenure; keep candidate identity based on address/session. |
| `pkg/relay/http.go` | Add the private conditional cleanup endpoint. |
| `pkg/relay/lookups.go` | Attach the authoritative epoch to resolved routes. |
| `pkg/relay/persistence.go` | Reject delayed route writes from a superseded epoch, including address reuse. |
| `pkg/relay/relay.go` | Read owner/epoch together and serialize conditional route/hold/replay cleanup with routing changes. |
| `pkg/relay/relay_test.go` | Keep slow/bounded lookup fault injection on the production fenced-read path. |
| `pkg/relay/tenure_cleanup_test.go` | Protect a newer lease/snapshot/persisted route/media path across address reuse, and a later-expired lookup epoch. |
| `docs/dev/ownership-expired-loss-verification.md` | Reviewable verification and cleanup record. |

## Limits

Registry, cleanup jobs, loss counters and bounded history are in memory and do not survive a control-plane restart. Built-in workers/control/relay must upgrade together for the placed-lease reply and conditional cleanup API. Custom adapters need the new optional capabilities to provide the same guarantee. Large-scale reconciliation latency, cloud/netns operation and the separate 60-second consent acceptance were not measured here. Default three-second leases and the envelope are unchanged; #100 was not implemented.

The sandbox denies `ps`, so host-wide process inventory is NOT-RUN. Owned Redis cleanup uses the tracked child plus Redis INFO's exact PID/port. Driver child managers completed their owned cleanup. Final owned-store shutdown confirmation is recorded below.

Cleanup verified: the dedicated unit/harness store reported PID `36662` and port `16499`, received `SHUTDOWN NOSAVE`, and its tracked child exited with code 0. The port can be rebound. The three final driver Redis logs also confirm graceful shutdown of PIDs `31331`, `32120`, and `33226`. No port 6379 instance was used or signalled. The throwaway base archive was removed after retaining red evidence. No commits or GitHub writes were made.
