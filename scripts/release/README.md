# Release compatibility contracts

`compatibility.json` is generated from encoded format versions, explicit contract
versions, versioned fixtures, and the reviewed `compatibility-policy.json`.
It contains no whole-source fingerprints. The unfiltered CI `compatibility` job
runs `check --base` against the PR base (or `HEAD^` on main) and is required by
`build`. Updating a golden and regenerating the manifest cannot bypass that gate.

- HTTP: bump `internal/privateapi.ContractVersion`, then run
  `RELAIS_UPDATE_HTTP_GOLDEN=1 go test ./internal/privateapi -run TestPrivateHTTPContractGolden`.
  Keep older versioned fixtures. The test reads actual handler registrations and
  decoder and written response types, query names, status and failure codes, and anonymous bodies.
- Lua: run `RELAIS_UPDATE_STORE_GOLDEN=1 go test ./pkg/sessionstore -run TestStoreScriptContractGolden`.
  Review every changed SHA. Bump `sessionstore.ScriptContractVersion`, or add a
  new reviewed `unchanged_tuples` note stating that persisted state **and wire
  behaviour** are unchanged. The field name is retained for manifest compatibility.
  Discovery covers the transitive local-package imports of `relais-worker`,
  `relais-control`, `relais-relay` and `relais-signal`. It includes every non-test
  Go file in those packages, across build tags. Diagnostic binaries such as
  `relais-load` are excluded unless a component imports their package.
  `NewScript`, `Eval`/`EvalSha` and `EvalRO`/`EvalShaRO` bodies must resolve
  statically. Raw Redis `Do`/`Process` commands must resolve to constants and
  cannot use `EVAL`, `EVALSHA`, `EVAL_RO`, `EVALSHA_RO`, `FCALL` or `FCALL_RO`.
- Keys: bump `sessionstore.KeyContractVersion`, then run
  `RELAIS_UPDATE_KEY_GOLDEN=1 go test ./pkg/sessionstore ./pkg/framecache -run TestRedisKeyContractGolden`.
- UDP: bump `relayleg.ContractVersion`, then run
  `RELAIS_UPDATE_UDP_GOLDEN=1 go test ./internal/relayleg -run TestUDPContractGolden`.
  Rejoin is an HTTP heartbeat token exchange; UDP fixtures cover caller headers
  and drain requests/acknowledgements.

Regenerate and verify:

```sh
go run ./scripts/release/compat generate --output scripts/release/compatibility.json
go run ./scripts/release/compat check --base BASE_COMMIT
```

A version bump records a contract change, not live interoperability. Before
publishing a supported pair, nominate its exact version and floors in the policy,
then run both upgrade and rollback on that candidate. `tested_peers: ["self"]`
refuses distinct builds. The required pair evidence is a passing **Release pair**
workflow run (Actions, `workflow_dispatch` with both versions; build the
unpublished side from its ref) or `make release-pair-source PAIR_A_VERSION=0.3.0
PAIR_A_SOURCE=<ref-or-checkout> PAIR_B_VERSION=0.3.1 PAIR_B_SOURCE=<ref-or-checkout>`.
It upgrades A to B and rolls back with live callers, runs the target archive's
own `relais-compat preflight` before every step, and keeps `release-pair-verdict.json`.
`make release-pair PAIR_A=<dir> PAIR_B=<dir>` runs existing archives;
`PAIR_FLAGS=-pair-expect-refusal=unsupported_pair` is the unnominated control. See the [operator procedure](../../deploy/aws/README.md#compatibility-gate-before-upgrade-or-rollback)
for the `0.3.0`/`0.3.1` nomination example and offline inventory sources.
