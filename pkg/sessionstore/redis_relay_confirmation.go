package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// The confirmation record lives in the lease hash next to the assignment, so
// one script reads or compares both atomically. A write compares the full
// assignment, the mode and the encoded attempt. It needs a live lease but not
// a worker epoch: control writes it, and a takeover keeps the record.
const relayConfirmationLua = `
local v=redis.call('HMGET',KEYS[1],'worker','expires','relay','relay_gen','relay_pending','relay_confirmation','relay_confirmation_state','relay_switched','relay_switched_at','relay_retired','relay_episode')
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
if not v[1] or tonumber(v[2])<=now then return {0} end
local slot,gen,pending,mode,state=v[3] or '',v[4] or '0',v[5] or '',v[6] or '',v[7] or ''
local switched,switchedAt,retired,episode=v[8] or '0',v[9] or '0',v[10] or '0',v[11] or ''
if ARGV[1]=='read' then return {1,slot,gen,pending,mode,state,switched,switchedAt,retired,episode} end
if slot~=ARGV[2] or gen~=ARGV[3] or pending~=ARGV[4] or mode~=ARGV[5] or state~=ARGV[6] or switched~=ARGV[9] or switchedAt~=ARGV[10] or retired~=ARGV[11] or episode~=ARGV[13] then return {-3} end
if ARGV[12]~='' and now>tonumber(ARGV[12]) then return {-4} end
if ARGV[15]~='' and now<tonumber(ARGV[15]) then return {-6} end
redis.call('HSET',KEYS[1],'relay_confirmation',ARGV[7])
if ARGV[8]=='' then redis.call('HDEL',KEYS[1],'relay_confirmation_state')
else redis.call('HSET',KEYS[1],'relay_confirmation_state',ARGV[8]) end
if ARGV[14]=='' then redis.call('HDEL',KEYS[1],'relay_episode')
else redis.call('HSET',KEYS[1],'relay_episode',ARGV[14]) end
return {1}
`

var relayConfirmationScript = redis.NewScript(relayConfirmationLua)

const clearInvalidRelayConfirmationLua = `
local v=redis.call('HMGET',KEYS[1],'worker','expires','relay_confirmation','relay_confirmation_state','relay_episode')
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
if not v[1] or tonumber(v[2])<=now then return 0 end
if ARGV[4]~='' and now>tonumber(ARGV[4]) then return -4 end
if (v[3] or '')~=ARGV[1] or (v[4] or '')~=ARGV[2] or (v[5] or '')~=ARGV[3] then return -3 end
redis.call('HDEL',KEYS[1],'relay_confirmation','relay_confirmation_state','relay_episode')
return 1
`

var clearInvalidRelayConfirmationScript = redis.NewScript(clearInvalidRelayConfirmationLua)

func (r *Redis) ClearInvalidRelayConfirmation(ctx context.Context, id string, notAfter time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	key := r.keys(id)[0]
	var fields []interface{}
	err := r.observe("relay_confirmation_clear_read", func() error {
		var err error
		fields, err = r.client.HMGet(ctx, key, "relay_confirmation", "relay_confirmation_state", "relay_episode").Result()
		return err
	})
	if err != nil {
		return classify("relay_confirmation_clear_read", err)
	}
	var saved [3]string
	for i, field := range fields {
		if field != nil {
			saved[i] = field.(string)
		}
	}
	if _, err := decodeRelayConfirmation(saved[0], saved[1], saved[2]); !errors.Is(err, ErrRelayConfirmationInvalid) && saved != [3]string{} {
		return ErrRelayConflict
	}
	bound := ""
	if !notAfter.IsZero() {
		bound = strconv.FormatInt(notAfter.UnixMilli(), 10)
	}
	var result int
	err = r.observe("relay_confirmation_clear", func() error {
		var err error
		result, err = clearInvalidRelayConfirmationScript.Run(ctx, r.client, []string{key}, saved[0], saved[1], saved[2], bound).Int()
		return err
	})
	if err != nil {
		return classify("relay_confirmation_clear", err)
	}
	switch result {
	case 1:
		return nil
	case 0:
		return ErrNotFound
	case -3:
		return ErrRelayConflict
	case -4:
		return ErrRelayAttemptExpired
	default:
		return fmt.Errorf("sessionstore: unexpected relay confirmation clear result %v", result)
	}
}

func (r *Redis) RelayConfirmation(ctx context.Context, id string) (RelayAssignment, RelayConfirmation, error) {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	var result []interface{}
	err := r.retryRead(ctx, "relay_confirmation_read", func() error {
		var err error
		result, err = relayConfirmationScript.Run(ctx, r.client, r.keys(id)[:1], "read").Slice()
		return err
	})
	if err != nil {
		return RelayAssignment{}, RelayConfirmation{}, err
	}
	if len(result) == 1 && result[0] == int64(0) {
		return RelayAssignment{}, RelayConfirmation{}, ErrNotFound
	}
	if len(result) != 10 {
		return RelayAssignment{}, RelayConfirmation{}, errors.New("sessionstore: malformed relay confirmation")
	}
	fields := make([]string, 9)
	for i := range fields {
		s, ok := result[i+1].(string)
		if !ok {
			return RelayAssignment{}, RelayConfirmation{}, errors.New("sessionstore: malformed relay confirmation fields")
		}
		fields[i] = s
	}
	gen, err := strconv.ParseUint(fields[1], 10, 64)
	a := RelayAssignment{Slot: fields[0], Generation: gen, Pending: fields[2]}
	if err != nil || !validRelayAssignment(a) {
		return RelayAssignment{}, RelayConfirmation{}, errors.New("sessionstore: malformed relay assignment fields")
	}
	c, err := decodeRelayConfirmation(fields[3], fields[4], fields[8])
	if err != nil {
		return RelayAssignment{}, RelayConfirmation{}, err
	}
	switched, e1 := strconv.ParseUint(fields[5], 10, 64)
	switchedAt, e2 := strconv.ParseInt(fields[6], 10, 64)
	retired, e3 := strconv.ParseUint(fields[7], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return RelayAssignment{}, RelayConfirmation{}, errors.New("sessionstore: malformed relay commit fields")
	}
	c.Commit = RelayCommit{Switched: switched, Retired: retired}
	if switchedAt > 0 {
		c.Commit.SwitchedAt = time.UnixMilli(switchedAt)
	}
	return a, c, nil
}

func commitArgs(c RelayCommit) []interface{} {
	at := "0"
	if !c.SwitchedAt.IsZero() {
		at = strconv.FormatInt(c.SwitchedAt.UnixMilli(), 10)
	}
	return []interface{}{strconv.FormatUint(c.Switched, 10), at, strconv.FormatUint(c.Retired, 10)}
}

func (r *Redis) CASRelayConfirmation(ctx context.Context, id string, at RelayAssignment, from, to RelayConfirmation, notBefore, notAfter time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromMode, fromState, fromEpisode, err := encodeRelayConfirmation(from)
	if err != nil {
		return err
	}
	toMode, toState, toEpisode, err := encodeRelayConfirmation(to)
	if err != nil {
		return err
	}
	if !validRelayConfirmationTransition(from, to) {
		return ErrRelayTransition
	}
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	var result []interface{}
	err = r.observe("relay_confirmation_cas", func() error {
		var err error
		bound := func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return strconv.FormatInt(t.UnixMilli(), 10)
		}
		args := append([]interface{}{"cas", at.Slot, strconv.FormatUint(at.Generation, 10), at.Pending, fromMode, fromState, toMode, toState}, commitArgs(from.Commit)...)
		args = append(args, bound(notAfter), fromEpisode, toEpisode, bound(notBefore))
		result, err = relayConfirmationScript.Run(ctx, r.client, r.keys(id)[:1], args...).Slice()
		return err
	})
	if err != nil {
		return classify("relay_confirmation_cas", err)
	}
	if len(result) != 1 {
		return errors.New("sessionstore: malformed relay confirmation result")
	}
	switch result[0] {
	case int64(1):
		return nil
	case int64(0):
		return ErrNotFound
	case int64(-3):
		return ErrRelayConflict
	case int64(-4):
		return ErrRelayAttemptExpired
	case int64(-6):
		return ErrRelayTooEarly
	default:
		return fmt.Errorf("sessionstore: unexpected relay confirmation result %v", result[0])
	}
}
