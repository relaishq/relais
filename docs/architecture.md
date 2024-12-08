# Relais Media Server Architecture

## Overview

Relais is a distributed media server that supports flexible ingress and egress of media streams through a plugin system. The architecture is designed for horizontal scalability and modularity.

## Core Components

### 1. Relais Core
- Manages sessions and coordinates between plugins
- Handles WebRTC signaling
- Provides control plane API

### 2. Plugin System
- Ingress plugins for media input
- Egress plugins for media output
- Transform plugins for media processing

### 3. Storage Backend
- Distributed storage for media frames
- Supports multiple implementations (Redis, Memory)

## Data Flow

1. Ingress plugins capture media and store frames
2. Transform plugins process stored frames
3. Egress plugins deliver frames to destinations

## Scaling

The system scales horizontally by:
- Running multiple plugin instances
- Using distributed storage
- Load balancing across core servers

## Security

- Session-based access control
- Optional authentication layer
- Secure WebRTC signaling