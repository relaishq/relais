# Stage-1 WebSocket evidence — 2026-10-16

PARTIAL — Room service, Go clients, real Relais two-way audio, WebSocket fault matrix, bounds and media regressions are VERIFIED. Browser runtime is NOT-RUN because isolated Chromium startup was denied by the sandbox. DataChannel integration is deferred to stage 2. Independent review remains with the orchestrator. Do not treat this as a full GO for Nvwa adoption.

## Identity and fixed conditions

Checkout initially clean: `spike/nvwa-room-app`, HEAD `7dfde0b7fbb52debf0651b5f23766e1ab09dd9fd`. No commit, push or GitHub post. Production changes are restricted to the opt-in worker agent selection and its regression test; agent v2/task code and Go modules remain untouched.

Final wefty run: `run_6ec4901b6c27126078f2d84e4936425f`, succeeded, 419.978 s ledger lifetime. Twelve independent local topologies; summed client-run durations 415.750 s. Each has dedicated Redis on a dynamically chosen non-6379 port, relay, control, signal, two workers, and a separate surviving room process. Child PIDs/ports and complete process logs are in the referenced private scratch directories. Allocator excludes duplicate TCP/UDP endpoints and ports 6379/9101/9201.

Every trial: two independent Go/Pion callers; A on w1 and B on w2 initially. 1,000 numbered operations per player, 30 Hz: 500 commands, 500 replaceable state messages. Payloads are fixed, Opus is 32 kb/s, 20 ms frames, A=440 Hz/B=880 Hz. Exact numbered-message app sizes after request-ID correction span 235–350 bytes. This is a single local-node observation without host load isolation; no reliability percentage, recovery p95, capacity or human-call-quality claim follows.

## Raw trial values

A/B are the server-bound p1/p2. State age is the maximum age of a latest state seen in any authoritative receipt, including the peer's last state during recovery. RTT includes the deliberate 150 ms lost-receipt timeout and query/retry path. Recovery spans A's last authoritative observation to the new join snapshot; B does not reconnect. Audio interruption is the maximum encoded-packet arrival gap across all sessions, not a perceptual quality metric.

| Run | RTT mean A/B ms | RTT max A/B ms | State age max A/B ms | Queue B / age ms | Recovery A ms | Audio gap A/B ms | Version |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| normal-1 | 0.605/0.581 | 8.330/8.283 | 80.317/80.963 | 952/2.174 | 0.000 | 41.878/39.964 | 2002 |
| normal-2 | 3.831/3.686 | 48.623/48.334 | 122.657/112.323 | 952/12.503 | 0.000 | 109.571/170.696 | 2002 |
| normal-3 | 3.121/3.042 | 36.652/32.612 | 110.083/111.585 | 952/10.404 | 0.000 | 127.297/139.279 | 2002 |
| disconnect-1 | 1.343/1.109 | 119.226/122.120 | 343.156/424.393 | 952/25.324 | 296.501 | 495.516/420.604 | 2003 |
| disconnect-2 | 2.693/2.814 | 51.843/64.729 | 1035.571/1098.263 | 947/18.298 | 1008.236 | 1298.702/1216.493 | 2003 |
| disconnect-3 | 0.960/0.880 | 28.099/23.843 | 367.623/359.793 | 947/4.018 | 358.567 | 555.384/482.900 | 2003 |
| worker-kill-1 | 0.574/0.559 | 8.729/16.000 | 67.950/76.135 | 947/9.343 | 39.532 | 248.372/239.634 | 2003 |
| worker-kill-2 | 0.537/0.539 | 10.546/11.181 | 135.017/109.816 | 947/2.018 | 109.122 | 298.331/239.229 | 2003 |
| worker-kill-3 | 0.568/0.626 | 10.900/9.457 | 82.833/118.781 | 947/5.528 | 52.439 | 257.014/238.823 | 2003 |
| lost-receipt-1 | 0.813/0.666 | 151.219/24.626 | 166.957/86.455 | 952/6.410 | 0.000 | 83.326/67.722 | 2002 |
| lost-receipt-2 | 0.892/0.660 | 151.253/35.121 | 167.169/85.561 | 952/4.978 | 0.000 | 168.301/184.648 | 2002 |
| lost-receipt-3 | 0.715/0.537 | 151.422/12.986 | 167.425/117.262 | 952/1.288 | 0.000 | 52.458/51.193 | 2002 |

All twelve rows have: 1,000/1,000 completed numbered operations; **0/0 unrecovered app-message loss, 0/0 duplicate incoming receipts, 0/0 reordered incoming receipts**, and **0 duplicate command effects**. The independent exporter checks the complete incoming request-ID journal within each generation. Room-applied count is exactly 1,000 per trial. Normal/lost-receipt versions converge at 2002; reconnect trials converge at 2003. Both final state sequences are 1000. The independent export also verifies distinct Relais session IDs, unchanged room epoch, exact worker-kill PID ownership, generation 1 -> 2, and recovery of p1 state sequence 250 plus the original wave v1/start_tick=0 before further input. Every disconnect/kill row has A reconnect count 1 and B count 0.

Each lost-receipt trial separately records exactly one intentionally dropped receipt, one deduplicated retry, and one explicit changed-content conflict. The original result is queried before retry. Intentional receipt loss is distinct from unrecovered command loss. The room records application success before the dropped receipt.

`summary.json` has all per-player raw summary fields, including cut-to-recovered time, packet counts, wire-size range and final versions. `raw/*.json.gz` retains full per-operation RTT/results, received authoritative snapshots, connection generations, command IDs/sequences, session IDs, packet arrivals, and bounded received audio specimens. `manifest.json` links every compressed artifact to its original scratch file and SHA-256 hash. Uncompress with `python3 -c 'import gzip,sys;sys.stdout.buffer.write(gzip.open(sys.argv[1],"rb").read())' FILE.json.gz`.

## Audio proof

The two disjoint encoded tone references are included as `tone-440.ogg` and `tone-880.ogg`. Across the final matrix, **39,104 received packets matched the peer reference**, with **zero own-reference or unknown packets**. The exporter independently checked **240 raw received Opus specimens** against the opposite player's reference and excluded the self reference. This checks actual output bytes through `Pion caller → relay → real worker → legacy room-audio agent → room UDP bus → peer agent → relay → peer caller`.

The bridge substitutes the latest peer Opus packet, expires it after 100 ms and suppresses output when absent. It does not mix, retime or promise voice quality. The caller's own RTP clock is retained. References and recorded output bytes demonstrate A reaching B and B reaching A; self-echo cannot satisfy the disjoint reference check.

## Backpressure and buffering

The separately recorded real-socket pause test applied **80 producer commands**, delivered typed `slow_consumer` to the paused observer, and recorded pending normal replies peaking at **15349 bytes** with maximum queue age **526.208 ms**. A terminal error frame is reserved inside the 16 KiB total pending cap; the normal-reply queue metric excludes that terminal frame. The reserve is 1024 bytes for direct WS and 1281 bytes for framed upstream. The two media streams continued with **70/71** peer packets and maximum gaps **21.646/22.016 ms**. Raw receipts/audio specimens are in `raw/backpressure.json.gz`.

App limits: 1 KiB messages, 16 KiB pending including an in-progress normal write plus a reserved terminal error, 128 output entries, two identities, four upstream labels, 16 connections, 120 input messages/second per connection/identity, 4096 admitted results/player, 128-byte latest state, and two 1200-byte latest Opus packets. No unbounded replay/reassembly queue exists.

Additional observed media socket budget: relay public UDP and relay-leg UDP each log effective read=8 MiB and write=4 MiB (relay requests 16 MiB reads; OS supplies 8 MiB). Each of the two workers also logs effective read=8 MiB/write=4 MiB. These four configured socket budgets total 48 MiB. They are kernel-buffer capacity reports, not measured resident memory or occupied pending bytes. Application/WebSocket IO temporaries and Pion/TLS/SCTP internals are additional costs. Whole-process RSS, TCP socket occupancy, and SCTP buffering are NOT-MEASURED in stage 1. Do not describe 16 KiB as the entire stack budget.

## Failures and limits preserved

The preliminary matrix `run_a927ccaabbd2f3b05bea7ad15947ab80` failed one startup trial before clients: room and worker accidentally selected the same ephemeral UDP port. Eleven other trials completed. `initial-startup-failure.txt` preserves the original refusal. The allocator was fixed before the final 12/12 matrix; no failed sample was relabeled successful.

Both task-owned Chromium attempts used separate scratch profiles and distinct fake WAV capture. They aborted before page execution on `bootstrap_check_in`/`child_port_handshake` Permission denied (1100), with a denied Crashpad settings path as well. `browser-startup-limit.txt` preserves this evidence. No machine settings, user Chrome, shared DevTools daemon, port 9101/9201, or host network state was changed. No sandbox-bypassing retry through wefty was made. The browser page passed JavaScript syntax checking; join/actions/audio/fault/browser transport runtime remain NOT-RUN and need the parent executor.

Fault detection/reconnect in the matrix is script-driven. Worker kill uses the exact owned w1 PID and SIGKILL; room/Redis continue. The harness recreates A's app/media connections promptly. Direct WS is independent of the media worker and is intentionally rejoined for the shared recovery contract. This does not measure automatic failure-detection delay or original-channel survival. Room-crash persistence is not implemented: restart creates a new epoch and old clients receive explicit epoch reset.

Git fetch was denied at shared worktree metadata outside writable roots; no retry through wefty, branch switch, commit, push or PR occurred. No new dependency/module/toolchain, paid API, model generation, cloud or Nvwa source work was used. Supervisors completed child Process.Wait cleanup. An independent post-run PID sweep is NOT-RUN because spawning ps was denied; no sandbox bypass was attempted.

## Checks and next gate

VERIFIED — Race/short/count=1 experiment tests; full workerapp tests in memory; room dedup, rejected-result stability, conflict, generation takeover, old subscription stop, epoch reset, stale-state drop, leave fencing, ledger/queue/message limits and typed oversized frame; real upstream framing; two-Go-client WS E2E; actual two-way audio routing and peer removal; opt-in agent/default selection.

VERIFIED — Targeted default-media regressions: legacy echo snapshot restore/export/state identity; audio-only call; browser-like offer plus audio/video echo; Chrome offer answer. Go vet for changed packages, JavaScript syntax, and diff whitespace checks passed. Dedicated Redis is exercised by every process trial. Redis-specific unit subcases are not claimed unless explicitly configured with an owned instance.

PARTIAL — Complete stage-1 acceptance: browser runtime is blocked. NOT-RUN — independent review, W1 integration, Relais DataChannel runtime/media coexistence/move-drain-crash policy, formal Rust/Bevy/Nvwa clients, animation playback, TURN/Internet/mobile/Safari, cross-host/region, capacity, room-crash durability and original-channel migration.

Recommendation to the orchestrator: retain this bounded room contract and WS baseline, HOLD adoption until stage 2 plus isolated browser runtime and independent review. See `../STAGE2-HANDOFF.md` for merge points and exact W1 requirements.
