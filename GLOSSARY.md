# Relais glossary

## Checkpoint module

**Checkpoint module**:
The per-call authority for admitting resumable state, preserving its write reservations, and recognizing the exact state acknowledged by the session store. A checkpoint is a successful fenced store write; accepting local state or beginning a write is not a durable acknowledgement.
_Avoid_: Snapshot writer (covers only part of the concept)

## Worker observation

**Worker observation**: The control plane's knowledge of one registered worker's liveness, rejoin progress, drain state and placement eligibility. A failed status read leaves liveness unproven; heartbeat expiry establishes death.
_Avoid_: Worker health check, ownership authority

## Planned-move recovery

**Planned-move recovery**: The control plane's continuation of one call's transfer through rollback, confirmation or interruption, keeping ownership, media state and the relay hold together until the recipient runs or a terminal outcome is known. A confirmed live export remains distinct from checkpoint-based takeover.
_Avoid_: Move retry helper, crash-only recovery

## Accepted-event module

**Accepted-event module**:
The per-call authority for accepting one agent event with its application state, consumed input position and host effects as one complete local change. Local acceptance is distinct from a successful checkpoint.
_Avoid_: Effect publisher (covers only part of the concept)
