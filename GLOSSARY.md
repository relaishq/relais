# Relais glossary

## Checkpoint module

**Checkpoint module**:
The per-call authority for admitting resumable state, preserving its write reservations, and recognizing the exact state acknowledged by the session store. A checkpoint is a successful fenced store write; accepting local state or beginning a write is not a durable acknowledgement.
_Avoid_: Snapshot writer (covers only part of the concept)

## Worker observation

**Worker observation**: The control plane's knowledge of one registered worker's liveness, rejoin progress, drain state and placement eligibility. A failed status read leaves liveness unproven; heartbeat expiry establishes death.
_Avoid_: Worker health check, ownership authority
