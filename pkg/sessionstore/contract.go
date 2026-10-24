package sessionstore

// ScriptContractVersion identifies the persisted tuples and lifecycle semantics
// shared by the session, component, route, index, control, relay-lease and frame-cache scripts.
// Bump it for an incompatible change. A script-only optimization may retain it
// only with a reviewed note that persisted state and wire behaviour are unchanged in testdata/script-contract.json.
const ScriptContractVersion = 1

// KeyContractVersion identifies Redis key names, escaping and Cluster hash tags.
// Change it when the versioned sessionstore/framecache key fixtures change.
const KeyContractVersion = 1
