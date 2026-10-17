# How Relais compares

Relais targets one specific operation: replace a live media worker while the
caller keeps its WebRTC connection, and carry a compatible audio agent's
checkpointed state with it. That helps an operator deploy mid-call. It does not
make Relais a complete conferencing or voice-AI platform.

[Relais's guarantee matrix](guarantees.md) is the source for its results and
limits. It covers successful planned moves, worker crash takeover, relay restart,
same-host standby, store outages, and agent continuity under stated conditions.
It also records known rollback failures, untested cases, and explicit exclusions.
Custom agents are separate Go workers built with the agent SDK, which is on
`main` and not yet released. External side effects must tolerate repeated
input. Relais does not promise lossless recovery after every failure.

Checked 2026-10-15. This is a comparison of documented mechanisms, not runtime
testing of other products or a head-to-head benchmark. A media server, an agent
process, and the signaling connection can fail independently. Their recovery
contracts must be read separately. Missing evidence here does not prove a
product cannot implement a capability.

## Media server recovery

| Tool and scope | What its documentation establishes |
| --- | --- |
| LiveKit, self-hosted | On termination, it rejects joins to new rooms, accepts joins to active rooms, and waits for participants to leave before exiting. A room fits on one node. [Distributed deployment](https://docs.livekit.io/transport/self-hosting/distributed/). Checked 2026-10-15. |
| LiveKit connection recovery | The client tries connection resume; the documented recovery includes ICE restart. If resume fails, full reconnection republishes tracks and emits participant lifecycle events. This is not a guarantee that every server crash follows that fallback. [Connecting](https://docs.livekit.io/intro/basics/connect/). Checked 2026-10-15. |
| LiveKit Cloud, historical architecture | Its 2022 engineering article describes moving a participant to another server with a client ICE restart. Treat this as a dated architecture description, not the current contract for every media-server deployment. [Distributed mesh article](https://livekit.com/blog/scaling-webrtc-with-distributed-mesh). Checked 2026-10-15. |
| Jitsi | Jicofo removes failed or shutting-down bridge allocations and reinvites affected participants. Graceful shutdown includes a period for relocating remaining participants; it does more than wait for all calls to finish. Recovery still needs available infrastructure. [Jicofo implementation](https://raw.githubusercontent.com/jitsi/jicofo/master/jicofo/src/main/java/org/jitsi/jicofo/conference/JitsiMeetConferenceImpl.java), [bridge configuration](https://raw.githubusercontent.com/jitsi/jitsi-videobridge/master/jvb/src/main/resources/reference.conf). Checked 2026-10-15. |
| OpenVidu Elastic/HA | Media-node failure causes a documented interruption of about five seconds before rooms, participants, and tracks are reconstructed. Egress is interrupted. Single Node is not fault tolerant; Elastic is a PRO deployment type. [Fault tolerance](https://openvidu.io/latest/docs/self-hosting/production-ready/fault-tolerance/), [Elastic](https://openvidu.io/latest/docs/self-hosting/elastic/). Checked 2026-10-15. |
| Vonage Video | Session migration is opt-in, requires SDK 2.30.0 or later, and supports server rotation or manual REST-triggered migration. It defaults off; clients without it disconnect on rotation. The guide does not establish unchanged ICE/DTLS state. [Server rotation](https://developer.vonage.com/en/video/guides/server-rotation). Checked 2026-10-15. |

ICE restart repeats WebRTC network-path setup. Full reconnection creates a new
session connection. Relais's measured worker replacement keeps that caller
transport; it can still cause a media pause. A **drain** moves or waits out active
work before shutdown; a **takeover** restores work after its owner fails.

Vonage's rotation guide distinguishes ancillary services: automatic archives
restart automatically, while other archives, broadcasts, captions, and Experience
Composer need restart. SIP and Audio Connector continue. These are separate
from the media-connection contract. [Rotation details](https://developer.vonage.com/en/video/guides/server-rotation).
Checked 2026-10-15.

## Agent rollouts and state

| Tool and scope | What its documentation establishes |
| --- | --- |
| LiveKit Cloud agent rollouts | Old instances stop taking new sessions and can finish existing work within a one-hour Cloud termination window, after which they are terminated. This concerns agent instances, not media servers. [Managing deployments](https://docs.livekit.io/deploy/agents/managing-deployments/). Checked 2026-10-15. |
| LiveKit Agents runtime | The current documented default drain timeout is one hour. Unexpected agent disconnection is detected in approximately 15 seconds before replacement dispatch; that is not a bound on replacement readiness. The reviewed lifecycle pages do not promise automatic crash-checkpoint restoration. [Options](https://docs.livekit.io/agents/server/options/), [lifecycle](https://docs.livekit.io/agents/server/lifecycle/), [job lifecycle](https://docs.livekit.io/agents/server/job/). Checked 2026-10-15. |
| LiveKit application context | Applications can initialize conversation context and explicitly transfer it during agent handoffs. A replacement agent need not start without context. These facilities alone do not establish automatic process-crash restoration. [Chat context](https://docs.livekit.io/agents/logic/chat-context/), [handoffs](https://docs.livekit.io/agents/logic/agents-handoffs/). Checked 2026-10-15. |
| Pipecat Cloud deployments | Active instances keep their old image until their sessions finish. A routine deployment does not cut off those sessions. [Deployments](https://docs.pipecat.ai/pipecat-cloud/fundamentals/deploy). Checked 2026-10-15. |
| Pipecat client lifecycle | After disconnection, the client does not automatically reconnect into a new session. Applications can reconnect explicitly. With `disconnectOnBotDisconnect: false`, a bot disconnection can leave the client connected. This does not prove loss of application context. [Session lifecycle](https://docs.pipecat.ai/client/concepts/session-lifecycle). Checked 2026-10-15. |
| Pipecat distributed agents | Transport and remote agent processes can be separate; examples hand off conversation control through Redis or PostgreSQL messaging. This is relevant prior work for agent handoff while transport stays put. Those pages do not document transparent recovery after the transport owner itself crashes. [Distributed agents](https://docs.pipecat.ai/pipecat/learn/distributed-agents), [proxy agents](https://docs.pipecat.ai/pipecat/learn/proxy-agents). Checked 2026-10-15. |

Relais's [audio-agent contract](adr/0006-agent-interface.md) puts versioned
application bytes and consumed-input progress in the transport checkpoint.
A planned move restores the accepted state/progress pair; crash takeover restores
the checkpoint and reports state age and possible duplicate input. That is not
an exactly-once guarantee for payments, messages, or other external effects.
Speech/LLM-provider integration, video-agent input, and a validated resumable
PCM decoder are outside the [current guarantees](guarantees.md#explicit-non-guarantees).

## Adjacent recovery features

Cloudflare Realtime SFU's architecture describes servers learning negotiated
connection parameters from other servers. Its WebSocket media adapters retry a
disconnected backend for up to 15 seconds with bounded buffering. These are
relevant adjacent mechanisms; the reviewed sources do not establish Relais's
exact media-process-crash contract. [Anycast architecture](https://blog.cloudflare.com/cloudflare-calls-anycast-webrtc/),
[WebSocket adapter](https://developers.cloudflare.com/realtime/sfu/features/media-transport-adapters/websocket-adapter/).
Checked 2026-10-15.

Cloudflare RealtimeKit's October 12 release notes describe keeping healthy media
running during signaling-WebSocket reconnection. Signaling recovery alone is not
evidence of media-server takeover. [Web release notes](https://developers.cloudflare.com/realtime/realtimekit/release-notes/web-core/).
Checked 2026-10-15.

LiveKit's current region-pinning guide says pinning disables automatic failover
to another region. This documents a regional recovery feature, without supplying
a node-migration protocol contract. [Region pinning](https://docs.livekit.io/deploy/admin/regions/region-pinning/).
Checked 2026-10-15.

Twilio Video remains supported after the reversal of its announced end of life.
[Video FAQ](https://www.twilio.com/docs/video/faq).
Checked 2026-10-15.

## Choosing what to evaluate

Start with the failure you need to survive: an agent rollout, an agent crash,
a media-worker replacement, relay failure, or a regional outage. Then check
whether the application context, caller transport, and external effects have
separate recovery rules. Do not assume that all other servers force reconnection,
that every replacement agent loses its context, or that a passing Relais sample
is a production-readiness claim. No first/only claim or comparable performance
benchmark is established here.
