# Relais API Reference

## Media Worker Signaling

The media worker (`pkg/mediaworker`) serves WHIP-style signaling; `cmd/echo-demo` mounts it next to the demo page.

```
POST /calls
Body: SDP offer (Content-Type: application/sdp)
201 Created with the SDP answer; Location: /calls/{id}

DELETE /calls/{id}
Hang up
```

See `SignalingHandler` in `pkg/mediaworker/signaling.go` for the current contract. Workers that share a UDP socket use `Socket.SignalingHandler` instead, which sends a hangup to whichever worker owns the call.

The echo demo adds two endpoints for its two workers:

```
POST /calls/{id}/move
Hand the live call over to the other worker; 200 with the move (timings, state size, held packets), 409 if it failed

GET /calls/{id}
The current owner, the move log and the owner's count of the caller's packets it could not decrypt
```

The control plane API (`/api/v1/sessions`, `/api/v1/plugins`) and the WebSocket signaling endpoint (`/ws/signaling`) were served by the retired `relais-core` server and no longer exist.

## Plugin Development

Plugins must implement one of:
- IngressPlugin
- EgressPlugin
- TransformPlugin

See pkg/plugins/interface.go for details.