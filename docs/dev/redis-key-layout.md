# Process store key and slot audit

`P` is the configured shared prefix, which cannot contain braces. Ordinary
session IDs use `T = {sess:<id>}`. Session-store IDs containing braces or
starting with `~` use `T = {sess:~<base64url(id)>}`. Existing frame-cache keys
use hex instead of base64url for those unusual IDs; each frame operation is
single-key. This slice changes no persisted key names or tags.

| Family | Key after P | Hash tag / slot |
| --- | --- | --- |
| Ownership lease | `lease:T` | T |
| Encrypted snapshot | `state:T` | T |
| Recovery / publication fence | `record:T` | T |
| Per-session epoch fence | `epoch:T` | T |
| Authenticated routes | `routes:T` | T |
| Ended-session marker | `ended:T` | T |
| Encrypted replay frames | `frames:T` | T; unusual IDs use hex as noted above |
| Worker membership index | `worker:<AddrPort>` | No tag; one independent key |
| Global epoch allocator | `session_epoch` | No tag; one independent INCR |
| Relay tenure | `relay:{relay:<base64url(relay-key)>}` | `{relay:<base64url(relay-key)>}` |

Future per-session keys (#188) must share the session store's exact T encoding
when a script touches them together. Do not include worker membership or the
global epoch allocator in per-session scripts: they occupy independent slots.

| Script / atomic operation | Keys | Slot rule |
| --- | --- | --- |
| `sessionLua`: claim, transfer, get, indexed, settle, renew, release, state/checkpoint reads, state sequence/put | lease, state, record, epoch, routes, ended | All T |
| Worker-list pipelined EVALSHA | Same six keys per member | Each command uses its member's T |
| Route-restore owner/prune EVAL | Same six keys per session | Each command uses its session's T |
| Store clock TIME EVAL | Same six keys for routing | T; no data keys accessed by TIME |
| `putRouteLua` | lease, routes | T |
| `deleteRouteLua`, including scan cleanup | routes | Single key |
| Forget routes DEL | routes | Single key |
| `indexLua` prepublish, `indexRefreshScript` | worker index | Single key |
| Worker-list ZRANGE, prune ZREM | worker index | Single key |
| Epoch INCR | global epoch allocator | Single key |
| `relayLeaseLua`: claim/get/renew/transfer/activate/release | relay tenure | Single key |
| Frame append/current Lua, frame DEL | frame hash | Single key |

No transaction or atomic cross-session multi-key command exists in the process
store path. Pipelines contain independent routed commands, not a transaction.
Route scans run on all primaries; the worker index is a single routed key, with
member validation pipelined to the appropriate session primaries. No ownership
or route read uses replicas, nearest-node routing, or random-node routing.

`TestRedisScriptSlotSafety` and `TestRedisFrameScriptSlotSafety` enumerate these
scripts and use the production key builders with Redis CRC16/XMODEM slot
arithmetic, including adversarial session IDs. `TestClusterRoutesIndexesAndScriptReload`
checks the live server across every primary, flushes every primary's scripts,
reloads relay-tenure scripts on every primary, and promotes a cold-cache replica
with ownership/state/route/relay fencing intact.
