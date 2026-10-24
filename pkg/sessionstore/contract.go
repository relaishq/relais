package sessionstore

// ScriptContractVersion identifies the persisted tuples and lifecycle semantics
// shared by the session, component, route, index, control and relay-lease scripts.
// Bump it for an incompatible change. A script-only optimization may retain it
// only with a reviewed unchanged-tuples note in testdata/script-contract.json.
const ScriptContractVersion = 1
