package sessionstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// All global coordination fits one persistent hash. TIME is evaluated on its
// primary. Only numeric millisecond timestamps are ordered; epochs are strings.
const controlLeaseLua = `
local op,holder,epoch,ttl=ARGV[1],ARGV[2],ARGV[3],tonumber(ARGV[4])
local t=redis.call('TIME'); local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local c=redis.call('HMGET',KEYS[1],'holder','epoch','expires')
local live=c[1] and c[1]~='' and tonumber(c[3])>now
local function reply() return {1,c[1],c[2],c[3]} end
if op=='claim' then
 if live then
  if c[1]==holder then
   c[3]=string.format('%.0f',now+ttl); redis.call('HSET',KEYS[1],'expires',c[3]); return reply()
  end
  return {0}
 end
 redis.call('HINCRBY',KEYS[1],'epoch',1)
 c={holder,redis.call('HGET',KEYS[1],'epoch'),string.format('%.0f',now+ttl)}
 redis.call('HSET',KEYS[1],'holder',holder,'expires',c[3]); return reply()
end
if not live or c[1]~=holder or c[2]~=epoch then return {2} end
if op=='renew' then
 c[3]=string.format('%.0f',now+ttl); redis.call('HSET',KEYS[1],'expires',c[3]); return reply()
elseif op=='release' then redis.call('HSET',KEYS[1],'holder','','expires','0'); return {1}
elseif op=='records' then return {1,redis.call('HGETALL',KEYS[1])}
elseif op=='put' then
 local field='record:'..ARGV[5]
 if ARGV[6]=='' then redis.call('HDEL',KEYS[1],field); return {1} end
 if redis.call('HEXISTS',KEYS[1],field)==0 then
  if redis.call('HLEN',KEYS[1])>=tonumber(ARGV[7])+3 then return {3} end
 end
 redis.call('HSET',KEYS[1],field,ARGV[6]); return {1}
end
return {4}
`

// The body and candidate are opaque JSON bytes to Lua, so uint64 epochs never
// pass through cjson/float conversion. Token checks fence delayed journal writes.
const controlIntentLua = `
local op,token=ARGV[1],ARGV[2]
local current=redis.call('HGET',KEYS[1],'token')
if op=='get' then return redis.call('HMGET',KEYS[1],'body','candidate') end
if op=='put' then
 if current and current~=token then return 0 end
 redis.call('HSET',KEYS[1],'token',token,'body',ARGV[3]); return 1
end
if current and current~=token then return 0 end
if op=='delete' then redis.call('DEL',KEYS[1]); return 1 end
if not current then return 0 end
if op=='candidate' then redis.call('HSET',KEYS[1],'candidate',ARGV[3]); return 1 end
return 0
`

var controlLeaseScript = redis.NewScript(controlLeaseLua)
var controlIntentScript = redis.NewScript(controlIntentLua)

func (r *Redis) controlKey() string { return r.prefix + "control:{control}" }
func (r *Redis) intentKey(id string) string {
	return strings.Replace(r.keys(id)[0], r.prefix+"lease:", r.prefix+"control-intent:", 1)
}
func (r *Redis) controlOperation(ctx context.Context, op string, lease ControlLease, ttl time.Duration, extra ...interface{}) ([]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	args := []interface{}{op, lease.Holder, strconv.FormatUint(lease.Epoch, 10), ttl.Milliseconds()}
	args = append(args, extra...)
	reply, err := controlLeaseScript.Run(ctx, r.client, []string{r.controlKey()}, args...).Slice()
	if err != nil {
		return nil, classify("control_"+op, err)
	}
	if len(reply) == 0 {
		return nil, errors.New("sessionstore: empty control reply")
	}
	switch reply[0] {
	case int64(0):
		return nil, ErrLeaseHeld
	case int64(2):
		return nil, ErrLeaseLost
	case int64(3):
		return nil, ErrControlLimit
	}
	if reply[0] != int64(1) {
		return nil, errors.New("sessionstore: invalid control reply")
	}
	return reply, nil
}
func decodeControlLease(reply []interface{}) (ControlLease, error) {
	if len(reply) != 4 {
		return ControlLease{}, errors.New("sessionstore: invalid control lease reply")
	}
	holder, ok := reply[1].(string)
	if !ok {
		return ControlLease{}, errors.New("sessionstore: invalid control holder")
	}
	epoch, err := strconv.ParseUint(fmt.Sprint(reply[2]), 10, 64)
	if err != nil {
		return ControlLease{}, err
	}
	expiry, err := strconv.ParseInt(fmt.Sprint(reply[3]), 10, 64)
	return ControlLease{Holder: holder, Epoch: epoch, ExpiresAt: time.UnixMilli(expiry)}, err
}
func (r *Redis) ClaimControl(ctx context.Context, holder string, ttl time.Duration) (ControlLease, error) {
	if !validControl(holder, ttl) {
		return ControlLease{}, errors.New("sessionstore: invalid control lease")
	}
	reply, err := r.controlOperation(ctx, "claim", ControlLease{Holder: holder}, ttl)
	if err != nil {
		return ControlLease{}, err
	}
	return decodeControlLease(reply)
}
func (r *Redis) RenewControl(ctx context.Context, lease ControlLease, ttl time.Duration) (ControlLease, error) {
	if !validControl(lease.Holder, ttl) {
		return ControlLease{}, errors.New("sessionstore: invalid control lease")
	}
	reply, err := r.controlOperation(ctx, "renew", lease, ttl)
	if err != nil {
		return ControlLease{}, err
	}
	return decodeControlLease(reply)
}
func (r *Redis) ReleaseControl(ctx context.Context, lease ControlLease) error {
	_, err := r.controlOperation(ctx, "release", lease, 0)
	return err
}
func (r *Redis) ControlRecords(ctx context.Context, lease ControlLease) (map[string]json.RawMessage, error) {
	reply, err := r.controlOperation(ctx, "records", lease, 0)
	if err != nil {
		return nil, err
	}
	if len(reply) != 2 {
		return nil, errors.New("sessionstore: invalid control records")
	}
	values, ok := reply[1].([]interface{})
	if !ok || len(values)%2 != 0 {
		return nil, errors.New("sessionstore: invalid control records")
	}
	out := map[string]json.RawMessage{}
	for i := 0; i < len(values); i += 2 {
		key := fmt.Sprint(values[i])
		if strings.HasPrefix(key, "record:") {
			data := json.RawMessage(fmt.Sprint(values[i+1]))
			if !controlRecordValid(strings.TrimPrefix(key, "record:"), data) {
				return nil, ErrControlLimit
			}
			out[strings.TrimPrefix(key, "record:")] = data
		}
	}
	if len(out) > MaxControlRecords {
		return nil, ErrControlLimit
	}
	return out, nil
}
func (r *Redis) PutControlRecord(ctx context.Context, lease ControlLease, key string, data json.RawMessage) error {
	if !controlRecordValid(key, data) {
		return ErrControlLimit
	}
	_, err := r.controlOperation(ctx, "put", lease, 0, key, string(data), MaxControlRecords)
	return err
}
func (r *Redis) PutControlIntent(ctx context.Context, intent ControlIntent) error {
	data, err := intentBody(intent)
	if err != nil {
		return err
	}
	return r.writeControlIntent(ctx, "put", intent.ID, intent.Token, data)
}
func (r *Redis) writeControlIntent(ctx context.Context, op, id, token string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	result, err := controlIntentScript.Run(ctx, r.client, []string{r.intentKey(id)}, op, token, data).Int()
	if err != nil {
		return classify("control_intent_"+op, err)
	}
	if result != 1 {
		if op == "put" {
			return ErrLeaseHeld
		}
		return ErrLeaseLost
	}
	return nil
}
func (r *Redis) DeleteControlIntent(ctx context.Context, id, token string) error {
	return r.writeControlIntent(ctx, "delete", id, token, nil)
}
func (r *Redis) journalCandidate(ctx context.Context, candidate Lease) error {
	intent, ok := ctx.Value(transitionIntentKey{}).(transitionIntent)
	if !ok {
		return nil
	}
	if intent.ID != candidate.SessionID {
		return errors.New("sessionstore: transition intent ID mismatch")
	}
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	return r.writeControlIntent(ctx, "candidate", intent.ID, intent.Token, data)
}
func (r *Redis) ControlIntents(ctx context.Context) ([]ControlIntent, error) {
	var mu sync.Mutex
	seen := map[string]bool{}
	out := []ControlIntent{}
	scan := func(ctx context.Context, node *redis.Client) error {
		var cursor uint64
		for {
			keys, next, err := node.Scan(ctx, cursor, escapeRoutePattern(r.prefix+"control-intent:")+"*", 100).Result()
			if err != nil {
				return err
			}
			for _, key := range keys {
				mu.Lock()
				duplicate := seen[key]
				seen[key] = true
				size := len(seen)
				mu.Unlock()
				if duplicate {
					continue
				}
				if size > MaxControlIntents {
					return ErrControlLimit
				}
				reply, err := controlIntentScript.Run(ctx, r.client, []string{key}, "get", "").Slice()
				if err != nil {
					return err
				}
				if len(reply) != 2 {
					return errors.New("sessionstore: invalid control intent read")
				}
				if reply[0] == nil {
					continue
				}
				body, ok := reply[0].(string)
				if !ok || len(body) > MaxControlRecordBytes {
					return ErrControlLimit
				}
				var intent ControlIntent
				if err := json.Unmarshal([]byte(body), &intent); err != nil {
					return err
				}
				if _, err := intentBody(intent); err != nil {
					return err
				}
				if key != r.intentKey(intent.ID) {
					return errors.New("sessionstore: control intent key mismatch")
				}
				if reply[1] != nil {
					if err := json.Unmarshal([]byte(fmt.Sprint(reply[1])), &intent.Candidate); err != nil {
						return err
					}
				}
				mu.Lock()
				out = append(out, intent)
				mu.Unlock()
			}
			cursor = next
			if cursor == 0 {
				return nil
			}
		}
	}
	var err error
	if cluster, ok := r.client.(*redis.ClusterClient); ok {
		err = cluster.ForEachMaster(ctx, scan)
	} else {
		err = scan(ctx, r.client.(*redis.Client))
	}
	if err != nil {
		return nil, classify("control_intents", err)
	}
	return out, nil
}

// SettleWorkerTransitions includes staged worker-index candidates, not merely
// committed ListByWorker results. This fences delayed creates after a restart.
func (r *Redis) SettleWorkerTransitions(ctx context.Context, worker netip.AddrPort) error {
	members, err := r.client.ZRange(ctx, r.indexKey(worker), 0, -1).Result()
	if err != nil {
		return classify("control_worker_candidates", err)
	}
	if len(members) > MaxControlIntents {
		return ErrControlLimit
	}
	for _, member := range members {
		var item struct {
			ID    string
			Epoch uint64
		}
		if err := json.Unmarshal([]byte(member), &item); err != nil {
			return err
		}
		id, err := base64.RawURLEncoding.DecodeString(item.ID)
		if err != nil {
			return err
		}
		if _, _, err := r.Settle(ctx, Lease{SessionID: string(id), Worker: worker, Epoch: item.Epoch}); err != nil {
			return err
		}
	}
	return nil
}
