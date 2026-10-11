# Soak results for #41

The soak mode (`relais-load -soak`, described in [soak.md](soak.md)) runs
continuous calls through a seeded chaos schedule of moves, drains and SIGKILLs.
It judges every event, the generator's saturation budget, memory per process
and bounded cleanup. The ticket's acceptance run (at least 50 calls for at least
one hour) needs more than the hosted 4-vCPU runner can drive. It runs on
dedicated hardware through the playbook in issue #85. Until that result is
posted, `acceptance` remains unproven.

## Hosted nightly variant (ubuntu-latest, 4 vCPU)

All runs use seed 41, an event every 30 seconds, three workers (worker 0 is the
memory sentinel from the final rules on), worker and relay floors of 8, and two
callers per generator process.

| Run | Code | Calls × duration | Events | Saturation | Outcome |
| --- | --- | --- | --- | ---: | --- |
| [38152777929](https://github.com/relaishq/relais/actions/runs/38152777929) | first soak mode | 8 × 10 min | 18 pass, 1 inconclusive (overlapped a saturated second) | 1.000% | Inconclusive. Under the final rules, 18/19 is 94.7%, below 95%. |
| [38154618392](https://github.com/relaishq/relais/actions/runs/38154618392) | 95% coverage rule | 6 × 10 min | 19/19 pass | 0% | Pass |
| [38159522631](https://github.com/relaishq/relais/actions/runs/38159522631) | sentinel, fenced rejoin, gap inventory | 6 × 10m10s | 20/20 pass | 0% | Fail: caller RSS projected 21.5 MiB/h (see below) |
| [38162635098](https://github.com/relaishq/relais/actions/runs/38162635098) | caller live-heap rule | 6 × 10m10s | 20/20 pass | 0% | **Pass**, `acceptance=false` (short run) |

In the latest run:

- every relay, control plane, Redis, sentinel worker and caller memory verdict
  that had enough data passed;
- callers were judged on live heap, at 2.8–3.4 MiB/h projected against a
  16 MiB/h limit;
- cleanup found both scan controls (`session_epoch` and the relay fencing
  hash), and every remaining per-session key carried a TTL within its bound;
- there were zero decryption failures and zero lost calls.

The only acceptance reason was scale.

## Caller memory

Run 38159522631 failed because all three caller (generator) processes grew in
RSS by 4–6 KiB/s. Over the same window their Go runtime reservations stayed
exactly constant, and their heap in use grew only about 1 KiB/s. The product
processes were flat.

The investigation in [caller-memory.md](caller-memory.md) found:

- no unbounded caller structure;
- heavy allocation churn: 523 MiB allocated in three minutes against a live
  heap change of about 0.5 MiB;
- about 2 MiB per caller released by an explicit `FreeOSMemory`.

That points to page retention in the test harness, not a leak. It is plausible
for the hosted Linux runs, not proven.

The callers are test tooling, so their leak signal is retained live memory.
Since that change they are judged on live heap after GC, and their RSS,
footprint and heap-in-use slopes are reported as diagnostics. Product processes
keep the RSS and heap gates, because RSS is what limits capacity. In the next
run, the caller RSS was flat as well.

## Mac development runs

Short local soaks (four calls, three minutes) on the development Mac exercised
every event kind, the sentinel, three token-based worker rejoins and bounded
cleanup. They were contended by other work on the same machine, and their
saturation reached 2–4.5%. They are smoke evidence only and are not reported as
results.
