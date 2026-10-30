package privateapi

// ContractVersion identifies private worker, relay and control HTTP payloads.
// Change it when their versioned request/response fixtures change. This is a
// release contract, not runtime negotiation or authentication.
const ContractVersion = 4
