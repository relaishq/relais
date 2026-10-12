# Caller authentication failures after rollback (#51)

The root cause is not established. Linux run 38174220164, lossy-dc move trial
15, recorded three caller auth-tag failures and no caller replay failures.
The uncertain-reply probe rolled back with an 8192-index sequence jump. The
provided report and service logs do not contain the failed packet headers.
They cannot establish the failed SSRC, protocol, sender, or rollover counter.

## In-process evidence

`TestHTTPUncertainRollbackDecrypt` uses real workers and private HTTP APIs,
the product-default relay buffer, a shared frame cache, and the lost committed
resume-reply seam. It waits for target audio before returning the transport
error. Fixed starts cover ordinary sequence numbers and outbound wraps.
Before adding diagnostics, 20 repetitions passed for each store: four
sequence starts per repetition, 80 memory and 80 throwaway Redis rollbacks.
A further 80 forced-target-media rollbacks and 80 immediate reply losses passed
with diagnostics under race, split evenly across the two stores. Five silent
barrier repetitions also passed on each store. The HTTP seam uses a shortened
20 ms hold timeout to admit target media while a reply remains in flight; the
shared frame cache is in memory in both session-store lanes. This does not
reproduce Linux loss/reorder behavior or prove a root fix.

## Diagnostic CI run

Dispatch CI with `network_profile_lane=lossy-dc-move`,
`private_route_tuning=true`, and `srtp_diagnostics=true`. Each dispatch runs
20 trials of 60 s by default. For a faster hunt, add `profile_trials=50` and
`profile_after=10s`: each trial still injects one uncertain-reply rollback and
still fails on any caller decryption failure, but it skips the 60 s consent
acceptance, so the verdict is inconclusive. Linux netns is required.

Results on 2026-10-11, all on Linux CI with private-route tuning:

| Diagnostics | Rollbacks | Caller decryption failures |
| --- | --- | --- |
| Off (CI history since the profile lane landed) | about 210 | 1 (run 38174220164, trial 15) |
| On, 20 trials × 60 s (11 runs) | 221 | 0 |
| On, 50 trials × 10 s (6 runs) | 300 | 0 |

The failure did not recur in 521 traced rollbacks. Either the rate is well
below 1 in 200, or tracing changes the timing enough to hide it. Active
hunting stopped there. The nightly `lossy-dc, move` job now runs with
diagnostics, and a failed run uploads the worker logs, so the next natural
occurrence leaves a trace.

The same diagnostic switch works for a local driver:

```sh
RELAIS_SRTP_DIAGNOSTICS=1 ./bin/crash-run -topology=netns \
  -profile=lossy-dc -scenario=move -runs=20 -after=60s
```

Diagnostics are opt-in and add work to the media path. Use ordinary runs to
judge timing once the fault is understood. They do not change any acceptance
threshold or suppress any existing failure counter.

`caller-report.json` gains `SRTPDiagnostics`. It authenticates copies of raw
received packets with a separate Pion context and the same default replay
windows. The existing `DecryptionFailures` from the caller's actual Pion
session remains authoritative. Check `Ready`, `Skipped`, and that both failure
counts agree before treating the shadow observations as the actual rejection
set. Unsupported or fragmented handshakes explicitly leave diagnostics
unavailable. At most 128 failure samples are retained; all failures are counted.

Each sample has protocol, SSRC, sequence, timestamp, receive time relative to
`StartedAt`, estimated index, previous authenticated index, and ciphertext
SHA-256. For SRTP auth failures only, fresh contexts try the adjacent rollover
counters without updating or delivering anything to either receiver.
`AuthenticatedIndex` is set only when that independent verification succeeds.
A different authenticated index proves a rollover estimate mismatch for that
ciphertext. An absent value does not prove wrong keys; corruption and other
index differences remain possible. SRTCP's explicit on-wire index is recorded.

Worker logs contain `SRTP_EGRESS` JSON when diagnostics are enabled. Match
`CiphertextSHA256` with the caller failure to identify the worker and lease
epoch that sent it. `Origin` distinguishes echo, custom-agent output, cached
video, and keyframe feedback. SRTP `Index` is reconstructed from persisted
track state; `ContextROC` is Pion's highest sender rollover counter, which may
differ from a reordered packet's counter. SRTCP `Index` is explicit on wire.
Use this state evidence together with the caller's authenticated-index probe
to locate a state/context disagreement. Headers and tenure show whether the
packet predates or follows rollback. A sent trace does not prove the relay delivered that packet;
the caller sample provides that second boundary. All diagnostic-run worker
logs are uploaded, including when the acceptance verdict passes.

Only metadata is retained. The DTLS debug-secret callback is consumed in
memory to derive shadow-receiver keys using Pion's TLS PRF and SRTP exporter.
Secrets and media payloads are never written to the diagnostic report or logs.

## Verification boundaries

`TestHTTPCallerCryptoDiagnostics` corrupts one real-worker audio, video and
SRTCP packet at the real caller socket on each store. It requires exactly
three auth failures in both the actual caller and shadow receiver and matches
metadata to the sender traces. `TestCryptoDiagnosticWrongROC` verifies that
an adjacent ROC authenticates the faulty packet without advancing the shadow
receiver; it covers AES-CM and AES-GCM and the evidence cap.

These test the instrumentation. There is no red/green regression for #51's
root cause yet. The original Linux reproduction remains required.
