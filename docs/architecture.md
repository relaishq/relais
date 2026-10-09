# Relais Media Server Architecture

## Overview

Relais is a distributed media server that supports flexible ingress and egress of media streams through a plugin system. The architecture is designed for horizontal scalability and modularity.

## Core Components

### 1. Media Worker (prototype)
- `pkg/mediaworker`: a minimal WebRTC endpoint built from Pion v4 component libraries (ICE-lite, DTLS-SRTP, RTP) instead of Pion's PeerConnection, so session state can later be exported and resumed on another worker
- WHIP-style signaling: one HTTP exchange per call
- Currently echoes the caller's Opus audio and VP8 video
- Tested through the call harness (`pkg/callharness`) and demonstrated by the browser echo demo (`cmd/echo-demo`)

### Relay (prototype)
- `pkg/relay`: one public UDP address in front of the media workers; every answer advertises it as the single host candidate
- Reads only STUN: a binding request's ICE username fragment is the session ID, which the relay looks up in the session-owner store (`pkg/sessionstore`) to find the owning worker
- Forwards DTLS, SRTP and SRTCP without parsing them, by a short-lived flow table (caller address to worker) that is only a cache of the store
- Workers bind only private sockets and reach callers through the relay over the relay leg, where a small versioned header carries the caller's address

The earlier `relais-core` server (Pion v3 signaling, session control plane API) has been retired.

### 2. Plugin System
- Ingress plugins for media input
- Egress plugins for media output
- Transform plugins for media processing
- Each plugin type runs in its own runner binary (`cmd/ingress-runner`, `cmd/egress-runner`, `cmd/transform-runner`)

### 3. Storage Backend
- Distributed storage for media frames
- Supports multiple implementations (Redis, Memory)

## Data Flow

Live calls terminate on the media worker and do not use storage yet. The plugin pipeline works on stored frames:

1. Ingress plugins capture media and store frames
2. Transform plugins process stored frames
3. Egress plugins deliver frames to destinations

## Scaling

The system scales horizontally by:
- Running multiple plugin instances
- Using distributed storage
- Running multiple media workers behind one relay address (planned: session handover between workers)

## Security

- Session-based access control
- Optional authentication layer
- DTLS-SRTP media encryption between caller and media worker