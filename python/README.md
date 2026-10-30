# Isolated Python loopback

This is ADR 0014 Appendix C slice 3. The worker connects to a Python bot that
listens on a literal loopback address. A static development token protects the
connection. Use this only on an isolated development machine. No packages or
releases are published by this slice.

Create the pinned environment inside the checkout:

```sh
python3.12 -m venv python/.venv
python/.venv/bin/python -m pip install -e python/relais-agent
python/.venv/bin/python -m unittest discover -s python/relais-agent/tests -v
```

Run the real-media proof, with one media worker and the in-process call harness:

```sh
RELAIS_TEST_PYTHON=../../python/.venv/bin/python go test -race -v -count=1 \
  -timeout=2m -run '^TestPythonBridgeHearLoopback$' ./pkg/callharness
```

The full test keeps caller input and consent alive for 65 seconds. It requires
at least 60 seconds of consent, the existing 4.5 second maximum consent silence,
zero caller and worker decryption failures, and all 150 tone packets. Python
decodes caller Opus and emits three seconds of 660 Hz Opus. The caller receives
it through Pion and the test decodes those exact received packets with PyAV.
It checks sample count, frequency, packet identities, sequence continuity,
500 ms maximum packet gap, and raw Python-ready-to-caller p95 ≤150 ms. Ready is
captured before queue or credit waits. Both processes use the same machine's
wall clock; this is local evidence. `-short` runs five seconds and does not
prove the consent window. Without `RELAIS_TEST_PYTHON`, the test skips.

To run the bot alone:

```sh
export RELAIS_BRIDGE_DEV_TOKEN=isolated-development-token
python/.venv/bin/python -m relais_agent --port 9102
```

Configure a development worker with its usual relay/control/store flags and:

```sh
-agent=bridge -agent-split-checkpoints \
-bridge-bot-url=ws://127.0.0.1:9102/bridge
```

Set `RELAIS_BRIDGE_DEV_TOKEN` to the same value in both shells. Prefer this
environment variable; the existing `--token` / `-bridge-dev-token` flags remain
available and override it. Tokens are never logged.

The bot validates Authorization with `hmac.compare_digest` and refuses browser Origins. Both endpoints
refuse non-loopback addresses; the worker also refuses DNS names, redirects,
URL credentials and query strings. Tokens never enter the URL. WebSocket
compression and library pings are disabled in the bot. There are no provider
calls, downloads, telemetry, subprocess supervision or machine-wide installs.

The development subset uses protocol 1 and binary framing from ADR 0014
Appendix B. Worker-to-bot `attached` follows the bot's `hello`; it contains the
socket's immutable scope and fresh-session limits. Output JSON uses `type`
with the receipt/credit fields at top level. The worker binds all incoming
packets to the socket scope. The bot sends `output_begin`, packets, `audio_end`
and a local checkpoint. It exercises idle `playback_finished`, `checkpointed`
and the captured host output acknowledgement. `replay_horizon(seq)` advances
the producer's no-retry boundary. `notice_gap(count)` reports overflow of
non-audio post-accept notifications; audio overflow uses `input_gap`.

The accepted input/notice outbox holds at most 32 records and 11 KiB of encoded
move-only state; the byte cap can reduce the packet count. Overflow reports
gaps. Each revision is immutable. Socket delivery is retired only by a later
accepted actor transaction. Save reads its cached export length; SaveMoveOnly
returns the exact repeatable image without waiting for the socket. Local
rollback and Restore preserve the accepted image. The tone
producer holds at most 32 packets and blocks on queue/absolute host credit.
The host socket writer has a one-second deadline. State callbacks do no socket
I/O. Rejected callbacks publish no transport events; checkpoint notices contain
the exact acknowledged pair. Frozen outbox images include pending input,
Control, playback notices and gap metadata, with validated lengths and session
identity. Socket buffers remain transient transport state.

Only fresh calls on one worker are supported. Worker moves, reattachment,
bot replacement, conversation recovery, process recovery, permit guards,
runner capacity, Pipecat integration and publication are later slices. This
loopback subset must not be used to claim those guarantees. No CI job is added.

The default Go race unit suite also runs a fake-bot WebSocket transport test:
`TestBridgeTransportSequentialCallsUseLeaseEpoch`. It covers two calls in a row,
a pre-claimed store lease, audio, checkpoint acknowledgement and protocol-error
socket closure. The optional Python test adds codec/decoding and timing evidence.
