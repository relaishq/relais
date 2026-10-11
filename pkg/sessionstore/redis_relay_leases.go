package sessionstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
)

// A single persistent hash retains fencing evidence and the monotone epoch.
// Redis TIME decides expiry; no client clock can authorize takeover.
const relayLeaseLua = `
local op,holder,epoch,ttl=ARGV[1],ARGV[2],ARGV[3],tonumber(ARGV[4])
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local c=redis.call('HMGET',KEYS[1],'holder','epoch','expires','forwarder','previous')
local function reply() return {1,c[1],c[2],c[3],c[4] or '',c[5] or '',tonumber(c[3])<=now and 1 or 0} end
if op=='get' then if not c[1] then return {3} end return reply() end
if op=='claim' then
 if c[1]==holder then return reply() end
 if c[1] and tonumber(c[3])>now then return {0} end
elseif op=='claim_dead' then
 if c[1]~=ARGV[5] or c[2]~=epoch or (c[4] or '')~=ARGV[6] or (c[5] or '')~=ARGV[7] then return {2} end
elseif op=='renew' and not c[1] then
 c={holder,epoch,string.format('%.0f',now+ttl),holder,''}
 redis.call('HSET',KEYS[1],'holder',holder,'epoch',epoch,'expires',c[3],'forwarder',holder,'previous','')
 return reply()
else
 if c[1]~=ARGV[5] then return {2} end
 if op=='renew' then
  -- Compare decimal epoch strings without Lua's floating-point precision loss.
  if #c[2]>#epoch or (#c[2]==#epoch and c[2]>epoch) then return {2} end
  if c[2]~=epoch then c[2]=epoch; redis.call('HSET',KEYS[1],'epoch',epoch) end
 elseif c[2]~=epoch then return {2} end
 if op~='renew' and op~='release' and tonumber(c[3])<=now then return {2} end
end
if op=='claim' or op=='claim_dead' or op=='transfer' then
 redis.call('HINCRBY',KEYS[1],'epoch',1)
 c[5]=c[1] or ''; c[1]=holder; c[2]=redis.call('HGET',KEYS[1],'epoch')
 c[3]=string.format('%.0f',now+ttl)
 redis.call('HSET',KEYS[1],'holder',c[1],'expires',c[3],'previous',c[5])
elseif op=='renew' then
 c[3]=string.format('%.0f',now+ttl)
 redis.call('HSET',KEYS[1],'expires',c[3])
elseif op=='release' then
 c[1]=''; c[3]='0'; c[4]=''; c[5]=''
 redis.call('HSET',KEYS[1],'holder','','expires','0','forwarder','','previous','')
elseif op=='activate' then
 c[4]=c[1]; c[5]=''
 redis.call('HSET',KEYS[1],'forwarder',c[4],'previous','')
end
return reply()
`

var relayLeaseScript = redis.NewScript(relayLeaseLua)

func (r *Redis) ClaimRelay(ctx context.Context, key string, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return r.relayOperation(ctx, "claim", RelayLease{Key: key}, p, ttl)
}
func (r *Redis) ClaimDeadRelay(ctx context.Context, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return r.relayOperation(ctx, "claim_dead", l, p, ttl)
}
func (r *Redis) RenewRelay(ctx context.Context, l RelayLease, ttl time.Duration) (RelayLease, error) {
	return r.relayOperation(ctx, "renew", l, l.Holder, ttl)
}
func (r *Redis) TransferRelay(ctx context.Context, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return r.relayOperation(ctx, "transfer", l, p, ttl)
}
func (r *Redis) ActivateRelay(ctx context.Context, l RelayLease) (RelayLease, error) {
	return r.relayOperation(ctx, "activate", l, l.Holder, time.Millisecond)
}
func (r *Redis) ReleaseRelay(ctx context.Context, l RelayLease) error {
	_, err := r.relayOperation(ctx, "release", l, l.Holder, time.Millisecond)
	return err
}
func (r *Redis) GetRelay(ctx context.Context, key string) (RelayLease, error) {
	return r.relayOperation(ctx, "get", RelayLease{Key: key}, RelayProcess{}, time.Millisecond)
}
func (r *Redis) relayOperation(ctx context.Context, op string, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	if op != "get" && !validRelay(l.Key, p, ttl) || l.Key == "" {
		return RelayLease{}, errors.New("sessionstore: invalid relay lease")
	}
	holder, _ := json.Marshal(p)
	expected, _ := json.Marshal(l.Holder)
	key := r.prefix + "relay:{relay:" + base64.RawURLEncoding.EncodeToString([]byte(l.Key)) + "}"
	reply, err := relayLeaseScript.Run(ctx, r.client, []string{key}, op, string(holder), strconv.FormatUint(l.Epoch, 10), ttl.Milliseconds(), string(expected), relayProcessEvidence(l.Forwarder), relayProcessEvidence(l.PreviousHolder)).Slice()
	if err != nil {
		return RelayLease{}, &TransientError{Op: "relay_" + op, Err: err}
	}
	if len(reply) == 0 {
		return RelayLease{}, errors.New("sessionstore: empty relay lease reply")
	}
	code, ok := reply[0].(int64)
	if !ok {
		return RelayLease{}, errors.New("sessionstore: invalid relay lease reply")
	}
	switch code {
	case 0:
		return RelayLease{}, ErrLeaseHeld
	case 2:
		return RelayLease{}, ErrLeaseLost
	case 3:
		return RelayLease{}, ErrNotFound
	}
	if code != 1 || len(reply) != 7 {
		return RelayLease{}, errors.New("sessionstore: invalid relay lease reply")
	}
	texts := make([]string, 5)
	for i := range texts {
		v, ok := reply[i+1].(string)
		if !ok {
			return RelayLease{}, fmt.Errorf("sessionstore: invalid relay lease field %d", i)
		}
		texts[i] = v
	}
	expired, ok := reply[6].(int64)
	if !ok {
		return RelayLease{}, errors.New("sessionstore: invalid relay expiry reply")
	}
	out := RelayLease{Key: l.Key, Expired: expired == 1}
	if err := json.Unmarshal([]byte(texts[0]), &out.Holder); texts[0] != "" && err != nil {
		return RelayLease{}, err
	}
	out.Epoch, err = strconv.ParseUint(texts[1], 10, 64)
	if err != nil {
		return RelayLease{}, err
	}
	expires, err := strconv.ParseInt(texts[2], 10, 64)
	if err != nil {
		return RelayLease{}, err
	}
	out.ExpiresAt = time.UnixMilli(expires)
	for i, p := range []*RelayProcess{&out.Forwarder, &out.PreviousHolder} {
		if texts[i+3] != "" {
			if err := json.Unmarshal([]byte(texts[i+3]), p); err != nil {
				return RelayLease{}, err
			}
		}
	}
	return out, nil
}

func (r *RedisOwners) ClaimRelay(ctx context.Context, k string, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return r.leases.ClaimRelay(ctx, k, p, ttl)
}
func (r *RedisOwners) RenewRelay(ctx context.Context, l RelayLease, ttl time.Duration) (RelayLease, error) {
	return r.leases.RenewRelay(ctx, l, ttl)
}
func (r *RedisOwners) TransferRelay(ctx context.Context, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return r.leases.TransferRelay(ctx, l, p, ttl)
}
func (r *RedisOwners) ActivateRelay(ctx context.Context, l RelayLease) (RelayLease, error) {
	return r.leases.ActivateRelay(ctx, l)
}
func (r *RedisOwners) GetRelay(ctx context.Context, k string) (RelayLease, error) {
	return r.leases.GetRelay(ctx, k)
}

func (r *RedisOwners) ReleaseRelay(ctx context.Context, l RelayLease) error {
	return r.leases.ReleaseRelay(ctx, l)
}

// Redis represents absent predecessor evidence as an empty hash field.
func relayProcessEvidence(p RelayProcess) string {
	if p == (RelayProcess{}) {
		return ""
	}
	data, _ := json.Marshal(p)
	return string(data)
}
func (r *RedisOwners) ClaimDeadRelay(ctx context.Context, l RelayLease, p RelayProcess, ttl time.Duration) (RelayLease, error) {
	return r.leases.ClaimDeadRelay(ctx, l, p, ttl)
}
