# Relais API Reference

## Control Plane API

### Sessions

```
POST /api/v1/sessions
Create a new media session

GET /api/v1/sessions/{id}
Get session information

DELETE /api/v1/sessions/{id}
End a session
```

### Plugins

```
POST /api/v1/plugins/{type}/start
Start a plugin instance

POST /api/v1/plugins/{type}/stop
Stop a plugin instance
```

## WebRTC Signaling

```
GET /ws/signaling
WebSocket endpoint for WebRTC signaling
```

## Plugin Development

Plugins must implement one of:
- IngressPlugin
- EgressPlugin
- TransformPlugin

See pkg/plugins/interface.go for details.