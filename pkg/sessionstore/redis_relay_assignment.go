package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// The assignment lives in the existing lease hash. Redis expiry, matching
// release, renew and worker transfer therefore share the lease lifecycle.
// Generations are decimal strings, never Lua doubles (which lose uint64 bits).
const relayAssignmentLua = `
local v=redis.call('HMGET',KEYS[1],'worker','epoch','expires','relay','relay_gen','relay_pending')
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
if not v[1] or tonumber(v[3])<=now then
 if ARGV[1]=='read' then return {0} else return {-2} end
end
local slot,gen,pending=v[4] or '',v[5] or '0',v[6] or ''
if ARGV[1]=='read' then return {1,v[1],v[2],v[3],slot,gen,pending} end
if v[1]~=ARGV[2] or v[2]~=ARGV[3] then return {-2} end
if slot~=ARGV[4] or gen~=ARGV[5] or pending~=ARGV[6] then return {-3} end
redis.call('HSET',KEYS[1],'relay',ARGV[7],'relay_gen',ARGV[8])
if ARGV[9]=='' then redis.call('HDEL',KEYS[1],'relay_pending')
else redis.call('HSET',KEYS[1],'relay_pending',ARGV[9]) end
return {1}
`

var relayAssignmentScript = redis.NewScript(relayAssignmentLua)

func (r *Redis) RelayOwner(ctx context.Context, id string) (Lease, RelayAssignment, error) {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	var result []interface{}
	err := r.retryRead(ctx, "relay_assignment_read", func() error {
		var err error
		result, err = relayAssignmentScript.Run(ctx, r.client, r.keys(id)[:1], "read").Slice()
		return err
	})
	if err != nil {
		return Lease{}, RelayAssignment{}, err
	}
	if len(result) == 1 && result[0] == int64(0) {
		return Lease{}, RelayAssignment{}, ErrNotFound
	}
	if len(result) != 7 {
		return Lease{}, RelayAssignment{}, errors.New("sessionstore: malformed relay assignment")
	}
	lease, err := decodeLease(id, result)
	if err != nil {
		return Lease{}, RelayAssignment{}, err
	}
	slot, ok1 := result[4].(string)
	genText, ok2 := result[5].(string)
	pending, ok3 := result[6].(string)
	gen, err := strconv.ParseUint(genText, 10, 64)
	assignment := RelayAssignment{Slot: slot, Generation: gen, Pending: pending}
	if !ok1 || !ok2 || !ok3 || err != nil || !validRelayAssignment(assignment) {
		return Lease{}, RelayAssignment{}, errors.New("sessionstore: malformed relay assignment fields")
	}
	return lease, assignment, nil
}

func (r *Redis) CASRelay(ctx context.Context, lease Lease, from, to RelayAssignment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRelayTransition(from, to) {
		return ErrRelayTransition
	}
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	var result []interface{}
	err := r.observe("relay_assignment_cas", func() error {
		var err error
		result, err = relayAssignmentScript.Run(ctx, r.client, r.keys(lease.SessionID)[:1],
			"cas", lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10),
			from.Slot, strconv.FormatUint(from.Generation, 10), from.Pending,
			to.Slot, strconv.FormatUint(to.Generation, 10), to.Pending).Slice()
		return err
	})
	if err != nil {
		return classify("relay_assignment_cas", err)
	}
	if len(result) != 1 {
		return errors.New("sessionstore: malformed relay assignment result")
	}
	switch result[0] {
	case int64(1):
		return nil
	case int64(-2):
		return ErrLeaseLost
	case int64(-3):
		return ErrRelayConflict
	default:
		return fmt.Errorf("sessionstore: unexpected relay assignment result %v", result[0])
	}
}

func (r *RedisOwners) RelayOwner(ctx context.Context, id string) (Lease, RelayAssignment, error) {
	return r.leases.RelayOwner(ctx, id)
}
