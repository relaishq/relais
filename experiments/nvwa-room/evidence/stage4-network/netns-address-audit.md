# Netns address audit after CI round 2

Base: `f88d418`, `spike/nvwa-room`. CI run `39002753889` confirmed Redis auth
and fail-fast; workers then rejected the private room-audio bus. This audit
covers executable source in `experiments/nvwa-room/`, `internal/spike/`, all
`pkg/mediaworker/datachannel*.go`, and `pkg/controlplane/datachannel.go`.
Search terms: `IsLoopback`, `127.0.0.1`, `localhost`, `loopback`,
`datachannel-upstream`, and signal/room/lobby URL and Origin validation.
Historical evidence and reports were not rewritten.

| File:line | Finding | Action and evidence |
|---|---|---|
| `internal/spike/roomaudio/agent.go:17` | Factory required a loopback UDP bus; this was the worker startup blocker. | **Changed.** Accept loopback or RFC1918 IPv4, retain a nonzero UDP port, reject public/unspecified/link-local/CGNAT/IPv6 ULA addresses. Malformed URI components now fail with an explicit message. All three RFC1918 ranges were observed red before the fix, then green. |
| `internal/spike/roomaudio/agent.go:45` | Audio callback has a 4 ms private bus deadline. | Unchanged. This is worker-to-room traffic, not caller-link RTT. The lossy-dc nominal private RTT is 1.5 ms; delayed/lost audio can still time out. This audit does not claim impaired audio timing success. |
| `experiments/nvwa-room/cmd/room/main.go:14` | HTTP/audio defaults use 127.0.0.1. | Unchanged defaults. Launcher supplies explicit private binds and `-spike-private-net`. |
| `experiments/nvwa-room/cmd/room/listen.go:8` | Existing private-CIDR exception worked, but failures incorrectly said only loopback was allowed. | **Changed.** Testable validation now reports loopback or the explicit private CIDR, rejects invalid/non-RFC1918 CIDRs, unspecified binds and out-of-CIDR binds. No broad private bind by default. |
| `experiments/nvwa-room/harness/topology.go:57`, `:65` | Ephemeral port probes bind host loopback. | Unchanged: probes close before service launch; they are not measured sockets or service destinations. Reserved ports and cross-protocol number exclusions remain. |
| `experiments/nvwa-room/harness/topology.go:130`, `:187` | Default address is loopback; netns overrides it per role. | Netns mapping unchanged. **Added an internal integration-test bind option** that applies the same launcher to an existing RFC1918 address without namespaces or interface changes. Default callers still pass no override. |
| `experiments/nvwa-room/harness/topology.go:224` | Authenticated Redis is needed for every private bind. | Existing auth construction reused by both namespace and private-address test paths. No password in public config/argv. |
| `experiments/nvwa-room/harness/topology.go:259` | Worker upstream is assembled from the room endpoint. | No URL change: it uses the private store/room address under netns. Actual WS/DC integration on private binds passed. |
| `experiments/nvwa-room/harness/topology.go:280`, `:292` | Signal bind, advertised certificate IP and room private CIDR come from configured addresses. | No netns change. TLS signaling and all services on private binds passed. |
| `experiments/nvwa-room/client/client.go:37`, `:53`, `:179` | WS, lobby and WSURL use supplied URLs. WS sends `Origin: http://localhost`. | Unchanged. The localhost value is an Origin header, not a dial address. Private lobby and WS delivery passed. |
| `experiments/nvwa-room/room/transport.go:359` | Origin permits same Host (including private IP/port), no Origin, or legacy localhost/127.0.0.1 headers. | Unchanged. New unit coverage verifies private same-host and legacy WS Origins, and rejects an unrelated public Origin. Actual private client/worker handshakes passed. |
| `experiments/nvwa-room/room/signal.go:24` | Browser proxy authorizes with the same Origin check and room capability. Signal address comes from configuration. | No loopback destination restriction. Unchanged; Go network driver signals directly. Browser proxy behavior is not newly claimed as runtime-tested. |
| `experiments/nvwa-room/media/call.go:30`, `:47`, `:74` | TLS trust comes from generated CA; requests use configured signal URL; deletion checks only relative `/calls/` shape. | Unchanged. Actual private-IP certificate/signaling, audio setup and deletion passed in all variants. |
| `pkg/mediaworker/datachannel.go:66`, `:272` | Upstream validator accepts ws/wss with private hosts. Dial uses configured upstream; `http://localhost/` is only the Origin header. | Unchanged. Both DC policies delivered commands and state through the real private upstream WS. No production defaults changed. |
| `pkg/mediaworker/datachannel_transport.go`, other `datachannel*.go` | No other loopback/address restriction. Localhost URLs in `datachannel_test.go` are isolated fixtures. | Unchanged. Reviewed transport/session files as well as grep matches. |
| `pkg/controlplane/datachannel.go:9` | Classifies SDP transport; has no endpoint URL/address check. | Unchanged. |
| `experiments/nvwa-room/cmd/network-driver/main.go:160` | Loopback cannot claim an impaired profile. | Unchanged: explicit and correct failure. Netns accepts the requested profiles and uses topology URLs. Other spike drivers use `harness.Start` for their existing local-only scenarios. |
| `experiments/nvwa-room/cmd/dcprobe/main.go:77`, `:90` | Signal defaults to loopback; `-signal` accepts a supplied HTTP(S) URL and rejects invalid scheme/host. | Unchanged. Standalone diagnostic, not the network lane. |
| `experiments/nvwa-room/web/app.js:23` | Browser WS uses `location.host`; lobby/media are relative URLs. | No literal loopback dial target. Unchanged; private HTTP browser media/secure-context behavior is outside the Go lane and was not run. |
| `experiments/nvwa-room/scripts/browser-smoke.py:20` | Browser sandbox exception is deliberately loopback-only. | **Changed message only:** private configurations explicitly direct the caller to `run-network.py`. No browser was launched or attached. |
| `experiments/nvwa-room/scripts/regression-stage2.py:7` | Owns a separate loopback Redis regression fixture. | Unchanged: local regression tool, never called by the netns lane. It has no netns option; unsupported arguments fail in argparse. |
| Spike unit tests and earlier README examples | Loopback fixtures/default examples and historical measurements. | Unchanged. These do not select network-lane service addresses. The stage-4 report documents private topology behavior. |

## Combined private-address check

`TestEveryServiceOnExistingPrivateAddress` at
`experiments/nvwa-room/harness/private_integration_test.go:22` starts the actual
Redis, relay, two workers, control, HTTPS signal and room on one existing local
RFC1918 address. The regular launcher supplies private Redis auth, listener
allowlists, advertised signal IP and upstream/room URLs. Each of WS, reliable DC
and unreliable DC must establish two callers, receive two-way peer audio with no
self/unknown payload, apply a command, and deliver a peer state update. This test
is wired into the spike CI preflight with `RELAIS_NVWA_TEST_BIN`.

No alias, route, firewall or network setting is changed. If no private interface
exists, this optional combined check skips; address-validator tests still run.
It passed locally for all three variants. This is private-address integration,
not Linux namespace, routing, loss, browser, or performance proof. Native netns
profile measurements still require the next CI dispatch.
