package sessionstore

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	codec "github.com/relaishq/relais/internal/componentcodec"
)

// componentKeys excludes legacy state and declares every accessed key, on the
// existing escaped tag. Lifecycle adds the six component keys to its old list.
func (r *Redis) componentKeys(id string) []string {
	old := r.keys(id)
	tag := strings.TrimPrefix(old[0], r.prefix+"lease:")
	return []string{old[0], old[2], old[3], old[4], old[5], r.prefix + "setup:" + tag, r.prefix + "checkpoint:" + tag, r.prefix + "work:" + tag + ":0", r.prefix + "work:" + tag + ":1", r.prefix + "history:" + tag + ":0", r.prefix + "history:" + tag + ":1"}
}
func (r *Redis) lifecycleKeys(id string) []string {
	return append(r.keys(id), r.componentKeys(id)[5:]...)
}

// Metadata offsets are frozen by componentcodec: five 41-byte references,
// epoch, app version, three bank bytes, owner length/string, setup generation.
const componentCommitLua = `
local owner,epoch,expected,m,b,seq,epochbytes=ARGV[1],ARGV[2],ARGV[3],ARGV[4],ARGV[5],ARGV[6],ARGV[7]
local function typ(k) return redis.call('TYPE',k).ok end
local function greater(a,b) return not b or #a>#b or (#a==#b and a>b) end
local function ref(s,i) return string.sub(s,2+i*41,42+i*41) end
local function revision(s,i) return string.sub(s,35+i*41,42+i*41) end
local function bank(s,i) return string.byte(s,219+i) end
local function validmeta(s)
 if #s<232 or #s>295 or string.byte(s,1)~=10 then return false end
 local n=string.byte(s,222)*256+string.byte(s,223)
 if n<1 or n>64 or #s~=231+n then return false end
 for i=0,2 do if bank(s,i)>1 then return false end end
 local zero=string.rep(string.char(0),8)
 if string.sub(s,-8)==zero or string.sub(s,207,214)==zero or revision(s,4)==zero then return false end
 for i=0,3 do if revision(s,i)==zero or revision(s,i)>revision(s,4) then return false end end
 return true
end
-- Preflight all key types and all argument bounds before the first mutation.
for i=1,11 do
 local expectedtype=({'hash','string','string','hash','hash','hash','hash','string','string','string','string'})[i]
 local actual=typ(KEYS[i]); if actual~='none' and actual~=expectedtype then return {-6} end
end
local lease=redis.call('HMGET',KEYS[1],'worker','epoch','expires','layout','state_seq','next_seq')
local time=redis.call('TIME'); local now=tonumber(time[1])*1000+math.floor(tonumber(time[2])/1000)
if lease[1]~=owner or lease[2]~=epoch or not lease[3] or tonumber(lease[3])<=now then return {-2} end
if lease[4] and lease[4]~='10' then return {-5} end
if #ARGV~=11 or not validmeta(m) or #epochbytes~=8 or #ARGV[11]~=8 or revision(m,4)~=ARGV[11] or string.sub(m,207,214)~=epochbytes or string.sub(m,224,-9)~=owner or #expected>295 or #b<30 or #b>131102 or not string.match(seq,'^[1-9][0-9]*$') or #seq>20 then return {-6} end
if not lease[6] or greater(seq,lease[6]) or not greater(seq,lease[5]) then return {-4} end
local old=redis.call('HGET',KEYS[7],'m') or ''
if old~=expected then return {-4} end
if old~='' and not validmeta(old) then return {-6} end
if old=='' and lease[4]=='10' then return {-6} end
if old~='' and string.sub(old,-8)~=string.sub(m,-8) then return {-6} end
if old=='' and string.sub(m,-8)~=epochbytes then return {-6} end
if old~='' then
 for i=6,11 do if typ(KEYS[i])=='none' then return {-6} end end
 if redis.call('HEXISTS',KEYS[6],'0')~=1 or redis.call('HEXISTS',KEYS[6],'1')~=1 then return {-6} end
end
for i=0,2 do
 local part=ARGV[8+i]
 local changed=old=='' or ref(old,i)~=ref(m,i) or (i>0 and string.sub(old,215,218)~=string.sub(m,215,218))
 if changed then
  if #part<30 or #part>131102 or revision(m,i)~=revision(m,4) or (old~='' and bank(m,i)==bank(old,i)) then return {-6} end
 else
  if #part~=0 or bank(m,i)~=bank(old,i) then return {-6} end
 end
end
if old~='' then
 local pairchanged=ref(old,1)~=ref(m,1) or ref(old,2)~=ref(m,2) or string.sub(old,215,218)~=string.sub(m,215,218)
 if pairchanged then if revision(m,3)~=revision(m,4) then return {-6} end
 elseif ref(old,3)~=ref(m,3) then return {-6} end
end
-- Initialization creates every bank; later commits only overwrite inactive banks.
if old=='' then
 redis.call('HSET',KEYS[6],'0','','1','')
 redis.call('PEXPIREAT',KEYS[6],lease[3])
 redis.call('HSET',KEYS[7],'b','','m','','t','')
 redis.call('PEXPIREAT',KEYS[7],lease[3])
 for i=8,11 do redis.call('SET',KEYS[i],string.char(0),'PX',math.max(tonumber(lease[3])-now,1)) end
 redis.call('HSET',KEYS[1],'layout','10')
end
for i=0,2 do
 local part=ARGV[8+i]
 if #part>0 then
  if i==0 then redis.call('HSET',KEYS[6],tostring(bank(m,i)),part)
  else redis.call('SET',KEYS[6+i*2+bank(m,i)],part,'PX',math.max(tonumber(lease[3])-now,1)) end
 end
end
-- All expiries precede publication; errors retain the previous referenced banks.
for i=6,11 do redis.call('PEXPIREAT',KEYS[i],lease[3]) end
redis.call('HSET',KEYS[1],'state_seq',seq)
-- publish: no fallible metadata write follows this single commit point.
local at=string.format('%.0f',now)
local packedtime=''; local n=now; for i=1,8 do packedtime=string.char(n%256)..packedtime; n=math.floor(n/256) end
redis.call('HSET',KEYS[7],'b',b,'m',m,'t',packedtime)
return {1,at,lease[3]}
`

const componentReadLua = `
local function typ(k) return redis.call('TYPE',k).ok end
if typ(KEYS[1])=='none' then return {0} end
if typ(KEYS[1])~='hash' then return {-6} end
local lease=redis.call('HMGET',KEYS[1],'worker','epoch','expires','layout')
local time=redis.call('TIME'); local now=tonumber(time[1])*1000+math.floor(tonumber(time[2])/1000)
if not lease[1] or not lease[3] or tonumber(lease[3])<=now then return {0} end
if lease[4]~='10' then return {-5} end
for i=6,11 do local want='string';if i<8 then want='hash' end;if typ(KEYS[i])~=want then return {-6} end end
local c=redis.call('HMGET',KEYS[7],'b','m','t')
if not c[1] or not c[2] or not c[3] or #c[2]<232 or #c[2]>295 then return {-6} end
local s,w,h=string.byte(c[2],219),string.byte(c[2],220),string.byte(c[2],221)
if s>1 or w>1 or h>1 then return {-6} end
local setup=redis.call('HGET',KEYS[6],tostring(s))
local work=redis.call('GET',KEYS[8+w]); local history=redis.call('GET',KEYS[10+h])
if not setup or not work or not history then return {-6} end
return {1,lease[1],lease[2],lease[3],c[1],c[2],c[3],setup,work,history,string.format('%.0f',now)}
`

var componentCommitScript = redis.NewScript(componentCommitLua)
var componentReadScript = redis.NewScript(componentReadLua)

func componentResult(result []interface{}, err error) ([]interface{}, error) {
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, ErrUnsafeCheckpoint
	}
	switch result[0] {
	case int64(0):
		return nil, ErrNotFound
	case int64(-2):
		return nil, ErrLeaseLost
	case int64(-4):
		return nil, ErrStateSuperseded
	case int64(-5):
		return nil, ErrCheckpointLayout
	case int64(-6):
		return nil, ErrUnsafeCheckpoint
	}
	return result, nil
}
func (r *Redis) runComponent(ctx context.Context, script *redis.Script, op, id string, args ...interface{}) ([]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	var result []interface{}
	fn := func() error {
		var err error
		result, err = script.Run(ctx, r.client, r.componentKeys(id), args...).Slice()
		return err
	}
	if op == "component_read" {
		err := r.retryRead(ctx, op, fn)
		return componentResult(result, err)
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = r.observe(op, fn)
		if err == nil || !definiteNotRun(redisKind(err)) || attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(20<<attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, classify(op, ctx.Err())
		case <-timer.C:
		}
	}
	return componentResult(result, classify(op, err))
}
func (r *Redis) readComponentRecord(ctx context.Context, id string) (componentRecord, Lease, time.Time, error) {
	result, err := r.runComponent(ctx, componentReadScript, "component_read", id)
	if err != nil {
		return componentRecord{}, Lease{}, time.Time{}, err
	}
	if len(result) != 11 {
		return componentRecord{}, Lease{}, time.Time{}, ErrUnsafeCheckpoint
	}
	lease, err := decodeLease(id, result)
	if err != nil {
		return componentRecord{}, Lease{}, time.Time{}, fmt.Errorf("%w: %w", ErrUnsafeCheckpoint, err)
	}
	m, err := codec.DecodeMetadata([]byte(fmt.Sprint(result[5])))
	if err != nil {
		return componentRecord{}, Lease{}, time.Time{}, fmt.Errorf("%w: %w", ErrUnsafeCheckpoint, err)
	}
	timestamp := []byte(fmt.Sprint(result[6]))
	if len(timestamp) != 8 {
		return componentRecord{}, Lease{}, time.Time{}, ErrUnsafeCheckpoint
	}
	stored := int64(binary.BigEndian.Uint64(timestamp))
	if stored < 0 {
		return componentRecord{}, Lease{}, time.Time{}, ErrUnsafeCheckpoint
	}
	now, err := strconv.ParseInt(fmt.Sprint(result[10]), 10, 64)
	if err != nil {
		return componentRecord{}, Lease{}, time.Time{}, ErrUnsafeCheckpoint
	}
	record := componentRecord{metadata: m, stored: time.UnixMilli(stored), sealed: Components{Transport: []byte(fmt.Sprint(result[4])), Setup: []byte(fmt.Sprint(result[7])), Work: []byte(fmt.Sprint(result[8])), History: []byte(fmt.Sprint(result[9]))}}
	return record, lease, time.UnixMilli(now), nil
}
func (r *Redis) ReadComponents(ctx context.Context, id string) (ComponentTuple, error) {
	record, lease, now, err := r.readComponentRecord(ctx, id)
	if err != nil {
		return ComponentTuple{}, err
	}
	return tupleFromRecord(r, id, record, lease, now)
}
func (r *Redis) CommitComponents(ctx context.Context, lease Lease, request ComponentCommit) (ComponentTuple, error) {
	if err := ctx.Err(); err != nil {
		return ComponentTuple{}, err
	}
	var record *componentRecord
	if request.Expected != nil {
		record = &componentRecord{metadata: *request.Expected}
	}
	return r.commitPrepared(ctx, lease, request, record)
}
func (r *Redis) commitPrepared(ctx context.Context, lease Lease, request ComponentCommit, record *componentRecord) (ComponentTuple, error) {
	for attempt := 0; attempt < 3; attempt++ {
		reserved, err := r.run(ctx, "sequence", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 0)
		if err != nil {
			return ComponentTuple{}, err
		}
		seq, err := sequenceNumber(fmt.Sprint(reserved[1]))
		if err != nil {
			return ComponentTuple{}, err
		}
		prepared, err := r.prepareComponents(lease, request, record, seq)
		if err != nil {
			return ComponentTuple{}, err
		}
		m, _ := prepared.record.metadata.Marshal()
		expected := []byte{}
		if request.Expected != nil {
			expected, _ = request.Expected.Marshal()
		}
		parts := componentBytes(prepared.record.sealed)
		for i := range parts {
			if !prepared.changed[i] {
				parts[i] = nil
			}
		}
		result, err := r.runComponent(ctx, componentCommitScript, "component_commit", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), expected, m, prepared.record.sealed.Transport, strconv.FormatUint(seq, 10), binary.BigEndian.AppendUint64(nil, lease.Epoch), parts[0], parts[1], parts[2], binary.BigEndian.AppendUint64(nil, seq))
		if err == nil {
			stored, e := strconv.ParseInt(fmt.Sprint(result[1]), 10, 64)
			if e != nil {
				return ComponentTuple{}, ErrUnsafeCheckpoint
			}
			prepared.record.stored = time.UnixMilli(stored)
			expires, e := strconv.ParseInt(fmt.Sprint(result[2]), 10, 64)
			if e != nil {
				return ComponentTuple{}, ErrUnsafeCheckpoint
			}
			lease.ExpiresAt = time.UnixMilli(expires)
			checkpoint, e := checkpointAt(prepared.record.stored, prepared.record.stored)
			return ComponentTuple{Components: cloneComponents(prepared.plain), Metadata: prepared.record.metadata, Lease: lease, Checkpoint: checkpoint, WrittenBytes: prepared.written, RotationDeferred: prepared.deferred}, e
		}
		if !errors.Is(err, ErrTransient) {
			return ComponentTuple{}, err
		}
		settleCtx, cancel := context.WithTimeout(context.Background(), transitionCommandLimit)
		settled, e := r.ReadComponents(settleCtx, lease.SessionID)
		cancel()
		if e != nil {
			if request.Expected == nil && errors.Is(e, ErrCheckpointLayout) {
				continue
			}
			return ComponentTuple{}, &TransientError{Op: "component_settle", Err: errors.Join(err, e)}
		}
		if metadataEqual(settled.Metadata, prepared.record.metadata) {
			settled.WrittenBytes = prepared.written
			settled.RotationDeferred = prepared.deferred
			return settled, nil
		}
		if request.Expected == nil || !metadataEqual(settled.Metadata, *request.Expected) {
			return ComponentTuple{}, ErrStateSuperseded
		}
	}
	return ComponentTuple{}, &TransientError{Op: "component_commit", Err: errors.New("settlement retry limit")}
}

// Inventory counts references in live published tuples, including the root and
// checkpoint authenticators, plus one per encrypted legacy snapshot. It scans
// every primary, never unreferenced banks. Plain legacy memory blobs have no key ID.
// Retirement also requires draining old writers; this is not a global transaction.
func (r *Redis) ComponentKeyInventory(ctx context.Context) (map[byte]uint64, error) {
	counts := map[byte]uint64{}
	seen := map[string]bool{}
	var mu sync.Mutex
	scan := func(ctx context.Context, node *redis.Client) error {
		for _, family := range []string{"checkpoint:", "state:"} {
			var cursor uint64
			for {
				keys, next, err := node.Scan(ctx, cursor, escapeRoutePattern(r.prefix+family)+"*", 100).Result()
				if err != nil {
					return err
				}
				for _, key := range keys {
					tag := strings.TrimPrefix(key, r.prefix+family)
					id := strings.TrimSuffix(strings.TrimPrefix(tag, "{sess:"), "}")
					if strings.HasPrefix(id, "~") {
						var e error
						id, e = decodeTagID(id)
						if e != nil {
							return e
						}
					}
					var refs []byte
					if family == "checkpoint:" {
						tuple, e := r.ReadComponents(ctx, id)
						if errors.Is(e, ErrNotFound) {
							continue
						}
						if e != nil {
							return e
						}
						for _, ref := range tuple.Metadata.References {
							refs = append(refs, ref.KeyID)
						}
					} else {
						result, e := r.run(ctx, "state", id, "", "", 0)
						if errors.Is(e, ErrNotFound) || errors.Is(e, ErrCheckpointLayout) {
							continue
						}
						if e != nil {
							return e
						}
						blob := []byte(result[1].(string))
						if _, e = r.openLegacyState(id, blob, result[2].(string)); e != nil {
							return e
						}
						refs = append(refs, blob[1])
					}
					mu.Lock()
					if !seen[key] {
						seen[key] = true
						for _, id := range refs {
							counts[id]++
						}
					}
					mu.Unlock()
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
		}
		return nil
	}

	var err error
	if cluster, ok := r.client.(*redis.ClusterClient); ok {
		err = cluster.ForEachMaster(ctx, scan)
	} else {
		err = scan(ctx, r.client.(*redis.Client))
	}
	return counts, err
}

func decodeTagID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "~"))
	return string(b), err
}
