# Authenticated loopback moves

This is the development subset of ADR 0014 Appendix C item 4. A runner
registers with both workers. The bot opens its session WebSocket to the
owner, using a short-lived, single-use attach token issued by that worker.
A planned move keeps the Python process, bot generation, lineage and output
sequence. The old worker-dials-bot shortcut and `-bridge-bot-url` are retired.

Use this only on one isolated development machine. Each worker has one runner
slot. The reference runner serves one call per process, including its moves;
restart the runner before placing another call. The fixture generates a tone
and makes no provider or tool requests.
Provider permits, replacement, revocation and crash recovery are not supported.
The worker explicitly reports `provider_starts=false`; this subset cannot
authorize external work. A different bot, runner, generation or lineage is
refused during reattachment. A crash checkpoint with an uncertain output tail
is also refused.

Create the pinned environment inside the checkout:

```sh
python3.12 -m venv python/.venv
python/.venv/bin/python -m pip install -e python/relais-agent
PYTHONPATH=python/relais-agent python/.venv/bin/python -m unittest discover \
  -s python/relais-agent/tests -v
```

Configure two workers with their usual relay/control/store flags and separate
literal loopback listeners:

```sh
-agent=bridge -agent-split-checkpoints \
-bridge-url=ws://127.0.0.1:9102/bridge
```

Use port 9103 on the second worker. Set `RELAIS_BRIDGE_DEV_TOKEN` to the same
16–128 visible ASCII byte development token in all three processes. The
`-bridge-dev-token` and Python `--token` flags override it. Tokens do not enter
URLs or logs. Start the runner before placing the call:

```sh
python/.venv/bin/python -m relais_agent \
  --worker ws://127.0.0.1:9102/bridge \
  --worker ws://127.0.0.1:9103/bridge
```

The runner opens `/bridge/registry` on each configured worker. Protocol 1
`hello`, `reserve`, `reserved` and `assign` use the fields in ADR 0014 Appendix B.
This fixed development slot requires capacity 1 and config revision `1`.
An assignment expires after five seconds. The bot opens `/bridge` and sends
`attach` with the assigned session, epoch, bot, generation and token, plus its
last-issued output sequence. An invalid scope cannot consume another scope's
token. A valid token is consumed even if later attachment fails.

A fresh binding checkpoints the runner identity and host lineage before
`attached`. An exact move preserves the host's accepted input outbox and output
ledger. Reattachment transfers their routing scope to the new lease epoch.
The returned exact cut retires admitted identities; the fixture resends only
retained, proven-unadmitted bytes above it. Packed sent receipts retain their
conflict evidence. Frozen exports keep their original bytes.

Both worker routes require Authorization, a loopback peer, no browser Origin
and no URL query. The runner refuses DNS names, remote addresses, URL
credentials and redirects. Proxy discovery, compression and library pings are
disabled. Input/output framing, bounded outbox, local Save/Restore, absolute
credits, exact acknowledgements, `replay_horizon` and gap notices retain the
slice-3 rules. State callbacks perform no socket I/O.

The default Go test `TestBridgeRunnerMovesLiveCall` runs in `make check`.
It moves a real WebRTC call three times and checks every bot packet ID on the
wire. Token tests cover authentication, Origin, scope, expiry and replay.
The optional codec test adds two moves, all 150 tone packets, raw ready-to-caller
p95 ≤150 ms, zero decryption failures and the existing full consent thresholds:

```sh
RELAIS_TEST_PYTHON=../../python/.venv/bin/python go test -race -v -count=1 \
  -timeout=2m -run '^TestPythonBridgeMoveLoopback$' ./pkg/callharness
```

`-short` keeps five seconds of caller input and does not prove the 60-second
consent window. The new test reports local move gaps; the unchanged harness
suites own the pooled planned-move policy, baseline margin and uncertainty
checks. No hosted CI run or Pipecat compatibility result is claimed here.
