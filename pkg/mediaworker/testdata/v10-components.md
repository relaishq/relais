# v10 component codec fixtures (slice A)

These codecs are not selected by live calls, checkpoint writes, store APIs or
legacy decoders. `components.go` converts owned captures into four components
and assembles an internal restore input. A later slice must implement the v10
lifecycle and public/self-contained export envelope before enabling admission.

Each component starts with format 10 and kind (setup=1, transport=2, work=3,
history=4). Decoding is capped at 128 KiB, above the 64 KiB configurable working
ceiling plus framing and all proposed component caps; move-only export payloads
are separate. Collection counts must fit the minimum encoded bytes per element
before allocation. Raw byte payloads use a bulk-copy path. `component_binary.go` documents the binary grammar. Hex files contain
actual canonical wire bytes, not ciphertext or secret production data. Existing
v7/v9 fixtures are unchanged; `v8-components-source.json` pins the previously
synthesized v8 source layout. The setup fixture contains only existing test keys.

- Setup: session identity, ICE credentials, local certificate/private key, caller
  identity and all pinned Pion v3.1.8 DTLS fields except the three changing fields.
- Transport: nominated address; LocalEpoch, RemoteEpoch and SequenceNumber; SRTP
  profile, inbound counters and receive masks; all audio/video negotiation,
  rewrite, SRTP/SRTCP counters and replay floors; checkpoint observations;
  consumed-input progress and local agent revision. The bounded host subtree
  retains registration, intents/cursors, timers, Control receipts, quota and
  move-only presence/length/playback metadata in the canonical v9 host grammar.
- Work and history: opaque byte strings, without JSON/base64 expansion. Their
  shared app State.Version is in authenticated metadata, not either payload.

Setup and transport have independent canonical encodings. Reassembly uses the
same `session.resume` as v9: its existing `advanceDTLSSequence` reserves 64 on
crash restore; confirmed planned moves stay exact. There is no second jump rule.
The outgoing guard remains below Pion. Setup changes are an explicit refusal.
Assembly shares legacy snapshot invariants, then re-splits to require identical
component bytes. Null host aliases, expanded address spellings and IPv4-mapped
IPv6 aliases are refused; capture canonicalizes mapped addresses to IPv4. The
consumed-input progress layout is component-local and frozen. Decode/authentication
refusals share `errBadState`; legacy version errors retain their original class.

`component_auth.go` defines AES-256-GCM (2-byte version/key-ID header, 12-byte
random nonce, 16-byte tag), with HKDF separation by session, component and encryption/digest purpose.
HKDF info is the fixed `relais/session/v10/` domain followed by uint32 session
ID byte length, session ID bytes, one kind byte, uint32 purpose byte length and
purpose bytes. Both encryption keys and digest keys are per session; Transfer
retains the session ID and keys. Component and root/checkpoint digests are HMAC-SHA256, stored as
32 binary bytes. Root input is the length-prefixed version/work/history tuple.
Whole-checkpoint digest excludes its own digest field before sealing. Reads
recompute all digests/root after AEAD authentication. Retained setup/app
ciphertexts do not bind an owner epoch; the checkpoint binds its full metadata.

The maximum packed `m` layout is 295 bytes: format (1), five digest/key-ID/revision
references (205), owner epoch (8), app version (4), banks (3), owner length (2),
owner (64), setup generation (8). Store-generated `t` adds 8; b/m/t field names
add 3; protocol framing reserve adds 78: **384 bytes**. Store time is not predicted
or included in client AEAD associated data. Metadata is stored once as `m`, never
duplicated in encrypted payloads. Digests themselves are binary in this layout.

## Real encoded-size report

`go test -v ./pkg/mediaworker -run '^TestComponent(SizeBudgets|DemoWorstStateFitsWorkingAndHistory|EncodedAdmissionBoundary)$'`

The max-width two-stream fixture uses the full 2048-byte aggregate host cap,
IPv6 plus zone/port, maximum-width counters, epochs, times, rates and receive
masks, full 3072-byte raw work and 8192-byte raw history. Numeric binary widths
also cover counter growth. With the fixture's negotiated track identifiers:

| Item | Encoded bytes | Proposed cap |
| --- | ---: | ---: |
| Transport and host, encrypted | 2490 | 3676 with full work/reserve |
| Working, encrypted (3072 raw) | 3108 | included in hot |
| Hot, including metadata/framing | 5982 | 7168 |
| Setup, encrypted real ECDSA fixture | 1435 | 4096 |
| History, encrypted (8192 raw) | 8228 | 8448 |
| Reference reserve | 384 | 384 |
| Whole transaction | 15645 | 20480 |

**These are not universal legacy-state maxima.** Negotiated text, authenticated
SSRC counts and peer certificate chains have no corresponding finite aggregate
bounds today. `TestComponentEncodedAdmissionBoundary` grows a real transport
encoding to exactly 7168 hot bytes, admits it, then rejects 7169. The unwired
`componentBudgets.admit` requires explicit budgets and counts actual component
encodings; it sets no live defaults. The later activation slice must use actual
encoded admission, including setup and total transaction caps, rather than infer
that any legacy session fits or enforce a new two-SSRC maximum. Oversize data is
refused, never truncated. No numeric allocation revision is needed for the
measured profile; unconditional fit for arbitrary legacy state is not claimed.

`v10-demo-state.json` was captured by applying the exact initializer in
`examples/voice-agent/voice/pipeline_invariants_test.go` to its exported
State/Timings/Window types and calling json.Marshal, exactly as Save does. No core
package imports examples. Its 7087 bytes split into **2871 working + 4205 history**
(11 bytes of the enclosing history field are removed). The measured Demo hot
encoding is 5781 bytes with the same max-width transport/host profile. This is a
codec size proof, not the later SDK schema change or live-provider crash proof.
