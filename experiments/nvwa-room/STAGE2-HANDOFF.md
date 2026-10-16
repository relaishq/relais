# Stage 2 integration handoff

> Historical stage-1 handoff. W1 was merged at 866a07d; current results and remaining gates are in evidence/REPORT.md.

Stage 1 implements the room, app consumers, real Relais peer audio, direct WebSocket workload/fault recorder, and frozen upstream handler. DataChannel runtime remains NOT-RUN until W1 is merged. Browser startup is blocked in this worker sandbox, before page execution; the parent must supply an authorized isolated browser executor.

## Merge points

Only `pkg/workerapp/agent.go`, `pkg/workerapp/app.go`, and the added `room_audio_test.go` change production worker selection. W1 likely also changes `app.go`: retain both sets of flags and both `mediaworker.Config` pieces. W2 selects the legacy agent from `-agent` and `-room-audio-bus`; W1 adds the opt-in DataChannel config. Do not change agent v2, task handling or the ADR slice-5 code.

W1 must provide exactly:

- `-datachannel` and `-datachannel-upstream` flags, bounds 1024/16384/four channels.
- Opus media plus application SDP negotiation via authenticated `relais-signal` and real control/relay/worker ownership.
- One worker upstream WS per session; binary frozen open/message/close/end frames.
- Typed move/export refusal, drain reconnect-required behavior, and a persisted data-session marker that blocks unsafe crash resume even after the source is dead.
- Outbound reliable overflow closure and PR oldest-drop counters/budgets, including SCTP buffers and underlying transport buffers.

Room `/relais-upstream` framing has a real WebSocket unit test, including session matching; this is not a substitute for W1 runtime proof. Go `media.NewDC` and the browser already request `control` ordered/reliable plus `state` unordered/zero-retransmit channels, with one shared app protocol.

## First integration commands

After merging, rebuild the same local binaries and use the same fixed tones, workload and fault cuts. `cmd/topology -datachannel` configures W1's flags with the room upstream URL. `cmd/driver -datachannel` creates media plus DataChannels through `relais-signal`; it never starts a standalone Pion server. Its other flags remain the WebSocket values in README. First run a 40-message smoke, then 1,000 messages and the full paired matrix. The current matrix script intentionally runs only WebSocket; parameterize it explicitly for stage 2 and retain distinct output roots.

Run the browser helper in an executor that can launch the task-owned binary. Change the page query to `transport=dc` for the integrated browser case; use separate scratch profiles and fake WAV input for every player. Do not attach to a user-owned Chrome or the shared DevTools daemon. Do not bypass sandbox denials with wefty.

## Evidence and limits to preserve

- Room PID/epoch remain unchanged during transport cuts. Two distinct Relais session IDs are mapped to the two server-bound players.
- Old-generation input and subscriptions are fenced. Query/retry the same logical command ID after a new generation; changed content must conflict.
- New connections recover the authoritative action reference and original tick. No old state input is replayed. Room restart remains an explicit new epoch, not durable recovery.
- Both directions must match the other player's tone/encoded reference, with zero self-echo. The tiny UDP bridge is packet substitution for a spike, not audio-quality or mixing evidence.
- Worker-kill recovery in stage 1 is script-driven. WS does not depend on that worker; the harness intentionally rejoins WS alongside the replacement media session. Do not present this as a measurement of automatic failure detection.
- Browser functional/audio results, native Rust/Bevy, actual character playback, Internet/TURN, mobile/Safari, cross-host capacity, room-crash persistence and original-channel migration remain separate gates.
- The first matrix exposed a duplicate ephemeral UDP-port allocation before client startup. That raw failure is retained; the final allocator excludes duplicate TCP and UDP endpoints and reserved ports. Do not erase the failed trial from the report.
- `result.json` contains raw app observations/packet-arrival timestamps and bounded received Opus specimens. `scripts/export-evidence.py` checks receipt IDs within generations and checks raw received bytes against disjoint 440/880 Hz references. These local observations do not establish a reliability percentage or recovery p95.

Independent code review and final GO/HOLD/NO-GO belong to the orchestrator after stage 2. Stage 1 alone is PARTIAL for the overall dispatch because browser runtime is blocked and DataChannel integration is deferred by design.
