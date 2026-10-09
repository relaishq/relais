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

See `SignalingHandler` in `pkg/mediaworker/signaling.go` for the current contract.

The control plane API (`/api/v1/sessions`, `/api/v1/plugins`) and the WebSocket signaling endpoint (`/ws/signaling`) were served by the retired `relais-core` server and no longer exist.

## Plugin Development

Plugins must implement one of:
- IngressPlugin
- EgressPlugin
- TransformPlugin

See pkg/plugins/interface.go for details.