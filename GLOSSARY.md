# Relais glossary

## Worker observation

**Worker observation**: The control plane's knowledge of one registered worker's liveness, rejoin progress, drain state and placement eligibility. A failed status read leaves liveness unproven; heartbeat expiry establishes death.
_Avoid_: Worker health check, ownership authority
