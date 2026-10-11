# Planned-move latency (#87)

[ADR 0007](../adr/0007-private-route-tcp-recovery.md) adopts Linux private-route
TCP recovery tuning as deployment policy. See the [operator commands](private-route-tuning.md).
No move retry, fencing, rollback or media threshold changed.

## Native Linux A/B

Both runs used `6.17.0-1022-azure` (Ubuntu `6.17.0-1022.22-azure`, base
`6.17.13`), with twenty lossy-DC moves and 60 seconds of consent per trial.

| Run | Private route tuning | Verdict | Caller audio gap | Audio / video gap p95 |
| --- | --- | --- | --- | --- |
| [38169245053](https://github.com/relaishq/relais/actions/runs/38169245053) | Off | Fail | 19 at 25–38ms; one at 253ms | 37.6 / 37.5ms |
| [38169250273](https://github.com/relaishq/relais/actions/runs/38169250273) | `rto_min 5ms quickack 1` | Pass | All at 23–43ms | 43.1 / 47.0ms |

Controller paused move durations were 22.4–26.7ms plus one 238.9ms outlier
untuned, and 23.3–25.3ms tuned. These are distinct from caller audio gaps.
The tuned gap p95 was higher (43.1/47.0ms versus 37.6/37.5ms); treat this as
single-run variation within the unchanged thresholds, not a demonstrated
population p95 improvement.

The untuned slow move spent 224.7ms in resume and 2.3ms in export. Store
`TCPLossProbes`, `TCPLossProbeRecovery` and `TcpRetransSegs` rose by one;
worker-0 had one probe and retransmission. `TCPTimeouts` stayed zero.
Resume had four Redis commands and one proxied HTTP call. The untuned run
failed a separate barrier-resend probe; the outlier alone did not fail p95.
The results support TCP tail-loss-probe delay. ADR 0007 describes the checked
upstream kernel mechanism and the vendor-source limit.

## Retained diagnostics

Measured profile moves enable `RELAIS_MOVE_DIAGNOSTICS=1` only in disposable
child processes. Ordinary processes leave it off: no diagnostic Redis hook,
HTTP trace allocation, or diagnostic endpoint is installed. Enabled traces
use bounded memory and record metadata, never session IDs, Redis arguments,
state bytes or bodies. The read-only collection endpoint is on the existing
private listener. Fetches run after the move and ending TCP samples.

`Rows[].move_diagnostics` records phase HTTP/Redis call counts and timings,
full HTTP response-body timing, payload sizes, connection reuse, separate
proxy upstream calls, and seven namespace TCP recovery counters. Root trace
IDs separate acceptance moves from preliminary fault probes. Pipeline command
durations overlap; Lua-internal commands are excluded. TCP counters include
all namespace traffic, and sequential samples have separate timestamps; they
cannot identify the specific lost socket. HTTP byte counts exclude headers.
Failed or detached recovery traces, missing/reset counters, collection errors
and buffer drops remain explicit, never fabricated as zero. Diagnostics are
non-fatal and do not change the acceptance verdict.

The old `private endpoint 4 ... EOF` was reproducible: the HTTP collector
included Redis's private port. Collection now uses only the four HTTP
listeners; the store remains in private-reachability checks and TCP samples.
Both original A/B reports therefore have `complete: false`, even though their
available phase/counter evidence is useful. Native re-runs must verify complete
collection after this correction.

Reports include `kernel_release`, explicit `private_route_tuning` (true or
false), observed `private_routes`, the slowest paused move and its
longest call. Every netns run, including unshaped LAN crash trials, also prints
`PRIVATE_ROUTE_SETTINGS` and writes `private-route-settings.json` with its tuning
state, kernel and observed DC routes. Untuned routes retain their raw output;
absent explicit TCP metrics are unspecified, not reported as zero.
JSON durations are nanoseconds. Kernel provenance uploads as
`network-profile-kernel.log`. Host-side proxy TCP settings remain untouched.

## Re-runs

After committing and pushing, run the tuned lane twice and then the full matrix:

```sh
# Dispatch this twice, as separate runs.
gh workflow run ci.yml --repo relaishq/relais --ref investigate/87-move-latency \
  -f network_profile_lane=lossy-dc-move -f private_route_tuning=true

gh workflow run ci.yml --repo relaishq/relais --ref investigate/87-move-latency \
  -f network_profile_lane=all -f private_route_tuning=true

# Optional untuned comparison; a failure still fails the workflow.
gh workflow run ci.yml --repo relaishq/relais --ref investigate/87-move-latency \
  -f network_profile_lane=lossy-dc-move -f private_route_tuning=false
```

Netns tuning and manual input default on; scheduled runs keep the full matrix.
All profile verdicts are binding, including lossy-DC moves. Compare the raw
rows, fault probes, phase calls and TCP counters as well as p95. Local native
and Linux cross-vet/race checks cannot prove Linux namespace behavior or the
updated full matrix; those belong to native Linux CI.
