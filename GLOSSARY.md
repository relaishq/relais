# Relais glossary

## Worker observation

**Worker observation**: The control plane's knowledge of one registered worker's liveness, rejoin progress, drain state and placement eligibility. A failed status read leaves liveness unproven; heartbeat expiry establishes death.
_Avoid_: Worker health check, ownership authority

## Planned-move recovery

**Planned-move recovery**: The control plane's continuation of one call's transfer through rollback, confirmation or interruption, keeping ownership, media state and the relay hold together until the recipient runs or a terminal outcome is known. A confirmed live export remains distinct from checkpoint-based takeover.
_Avoid_: Move retry helper, crash-only recovery
