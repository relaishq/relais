# Redis frame cache

`framecache.Redis` implements the same complete-frame `Store` contract as
`Memory`. Workers in separate processes can read the current keyframe group
from Redis after takeover. The relay never receives the frame-cache key or
SRTP-decrypted video. Worker and control binaries use Redis for both session
snapshots and frames; the control plane deletes frames at hangup or call loss.

## Configuration and lifecycle

```go
frames, err := framecache.NewRedis(ctx,
    storage.RedisConfig{Addr: "127.0.0.1:PORT", Prefix: "deployment:"},
    key, framecache.Limits{}, framecache.RedisOptions{KeyID: 2,
        OldKeys: map[byte][]byte{1: previousKey}})
```

Supply a 32-byte master key, or pass nil to read `RELAIS_SESSIONSTORE_KEY`
(standard base64). The process binaries accept the same hex or base64 key as
the session store. Every worker and its control plane must use the same Redis
address, prefix, limits and key ring. The caller owns `Close`; close the frame
store after workers and control have stopped. Empty addresses and prefixes
containing braces are rejected. The process commands refuse port 6379.

`callharness.Options.FrameCache` selects a caller-owned store for either local
harness topology. Nil selects Memory. External topology owns its stores in the
separate processes. `relais-worker -frame-cache-off` disables writes and replay
while keeping the takeover PLI enabled.

## Binary records and encryption

The plaintext record begins with version 1. Integers are big endian:

- Length-prefixed track kind, track SSRC, source RTP timestamp, source SSRC and
  last observed echo timestamp.
- Length-prefixed binary arrival time (`time.Time.MarshalBinary`), preserving
  wall time and zone offset across processes; monotonic clock data is local.
- Keyframe byte, packet count, then each packet's uint16 source sequence,
  marker byte and uint32-length-prefixed payload (including VP8 descriptors).

Media records use no JSON or base64. The stored envelope contains a version
byte, key ID, fresh 96-bit random nonce, and AES-256-GCM ciphertext/tag. Encryption
is mandatory: these are the caller's own SRTP-decrypted pictures. The #10
snapshot master key derives a separate key for each session using HKDF-SHA256
with `relais/framecache/v1/SESSION_ID`, a distinct domain from session snapshots.
Length-prefixed session/track identity and envelope headers are authenticated.
Malformed or unauthenticated records fail the whole read. Readers also check
frame continuity and configured bounds; they never return a partial group.
Routing identity and group accounting (size/count/last sequence/timestamp) in
Redis hash fields are metadata, not encrypted media.

Distribute old and new keys to all readers before changing the active key ID.
Old keys decrypt only. Do not reuse a key ID until its old ciphertext expires
or is replaced, and rotate before any single session approaches GCM's random
nonce usage limit. This cache is advisory, not an authenticated durable history
or a protection against a Redis administrator rolling back an entire group.

## Atomic layout and retention

Each session has one Redis hash, `PREFIXframes:{sess:SESSION_ID}`. Track fields
use hex track-kind bytes and decimal SSRC to avoid aliases. A track's `b/n/s/t`
fields hold payload bytes, frame count, last sequence and timestamp; `f1..fn`
hold encrypted binary records. Unusual session IDs are escaped to avoid
braces changing the Cluster hash tag.

One Lua script per append validates continuity and atomically replaces a
keyframe group, appends a complete frame, or removes the whole group. An
incomplete frame is rejected before I/O. A sequence hole, non-forward timestamp,
byte overflow or frame overflow makes the group unavailable until the next
complete keyframe. The read script returns all frames atomically. Ambiguous
network failures are never retried: duplicate appends could damage continuity.

Defaults match Memory: 8 MiB of payload and 300 frames per track, with a 30 s
idle TTL. Every existing session hash gets its TTL refreshed by successful
append/read scripts, including a read of another track. `DeleteSession` deletes
the entire hash. Read and append therefore have session-wide idle semantics,
without track keys or indexes surviving deletion/expiry.

Redis does not apply Memory's global LRU byte/session caps. Per-track caps bound
active media retention and TTL removes idle sessions; total Redis use still
scales with active session/track count and includes packet/encoding overhead.
Deployments must provision capacity and choose an eviction/persistence policy;
this is not a fixed global memory ceiling. Redis Cluster keys are slot-safe;
this change's live integration verification uses standalone Redis.

## Packet path and slow Redis

Each worker has one ordered append consumer, with at most 32 complete frames
and 8 MiB of queued payload **including the append in flight**. The UDP reader
only transfers a completed frame to this queue. Saturation drops new appends,
never blocks media or allocates another goroutine. `ReplayStats` (also in the
worker's private status API) exposes `AppendDropped`, `AppendErrors`,
`AppendPending` and `AppendBytes`. A subsequent sequence hole invalidates the
cached group, so dropped work cannot produce a truncated replay group.

Append has a bounded context. Queued work checks session cancellation/fencing,
and local deletion serializes with append; shutdown discards the queue and
joins its consumer. The existing takeover `Current` budget remains 150 ms;
read timeout, authentication failure or missing data yields a cache miss and
PLI fallback. The cache remains advisory during Redis failure.

## Recovery measurements and limits

`make crash-run` now runs ten real-process SIGKILL trials **per mode**:
Redis cache plus PLI, then cache off plus PLI. Each trial preserves 60 s of
post-takeover media/consent observation. It prints first-decoded and first-live
latency from SIGKILL, first/live attribution, replay packet count, decryption,
connection and takeover checks, followed by a paired `CACHE_VS_PLI` table. The
cache-off trials require Keyframe attribution and zero replay packets. A
comparison that never decodes a cached picture fails explicitly.

The embedded 320x240/30 fps fixture is killed 1.550 s into sending (plus 7 ms
per trial, identical phases for both modes). This samples an admissible current
group under #9's unchanged default. Recovery is measured at the real caller by
PictureID **and exact encoded payload**, with pure-Go keyframe decode and full
ffmpeg VP8 decode; it does not trust worker replay flags. A short check is:

```sh
make crash-run CRASH_FLAGS='-runs 2 -after 2s'
```

That prints DEVELOPMENT RUN and does **not** verify 60 s consent acceptance.
`-compare-cache=false -frame-cache-off` selects one PLI-only mode. `-sigterm`
retains the planned-drain check and does not run crash comparison.

The finding from #9 still applies: in this one-worker echo topology, replay
restores a picture sent **before** the crash. Live video still needs a keyframe.
Admission remains half a frame interval at 10 Mbps, about 21 KB at 30 fps
including RTP/SRTP overhead. Typical 720p groups exceed that budget and fall
back to PLI. Cached-picture latency is not live-video recovery latency, and
these loopback fixture timings do not establish a browser or production SLA.
The caller responds to PLI by rewinding an already encoded keyframe on its next
33 ms tick; these comparisons include no real encoder startup delay.

## Verification

Set `RELAIS_TEST_REDIS_ADDR` to a throwaway loopback Redis on a free non-6379
port and `RELAIS_TEST_REDIS_REQUIRE=1`; disable persistence and place its dir
under `bin/`. The shared `TestStore*` suite runs unchanged against Memory and
Redis, including expiry, bounds, copying, deletion, references and serial wrap.
Redis-specific tests cover binary truncation, at-rest authentication, session
and track binding, rotation, independent clients and a real client's read
socket deadline. Worker tests exercise queue saturation through the decrypted
packet path, continuing echo and discarding queued work at hangup. The Redis
variants of frame-cache harness modes and interrupted-keyframe tests select
`Options.FrameCache` explicitly.
