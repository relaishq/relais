package sessionstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/storage"
)

// ErrTransient means Redis could not establish an operation's outcome. Callers
// must retain recovery state and re-read ownership before making a decision.
var ErrTransient = errors.New("sessionstore: transient Redis failure")

// TransientError preserves the underlying network/context error and identifies
// an uncertain operation without falsely reporting a lost or missing lease.
type TransientError struct {
	Op  string
	Err error
	// Candidate identifies an ownership transition whose settlement also failed.
	Candidate *Lease
}

func (e *TransientError) Error() string        { return fmt.Sprintf("sessionstore: %s: %v", e.Op, e.Err) }
func (e *TransientError) Unwrap() error        { return e.Err }
func (e *TransientError) Is(target error) bool { return target == ErrTransient }

// Redis implements Store on Redis 5+ (effects replication permits writes after
// TIME). Per-session keys share {sess:ID}; lease and ciphertext expire together.
// Record and epoch metadata expire after the lease plus Retention. Never reuse
// a cleared namespace while old commands can still be in flight; retention must
// exceed the maximum command lifetime. The one global epoch counter persists.
// Index members are prepublished; listing leaves young entries alone and fences
// uncommitted members older than IndexGrace
// before pruning them, so a delayed transition cannot commit without an index.
// Network retries are disabled. Reads, settlement and idempotent index writes
// retry; mutations retry only definite-not-run Redis responses.
// Snapshots use per-session HKDF-SHA256 keys, version/key-ID headers and a
// fenced per-put sequence authenticated with the session ID and header.
type Redis struct {
	client           redis.UniversalClient
	prefix           string
	retention        time.Duration
	indexGrace       time.Duration
	maintenanceMu    sync.Mutex
	maintenance      sync.WaitGroup
	maintenanceSlots chan struct{}
	closed           bool
	activeKey        byte
	keysByID         map[byte][]byte
}

// RedisOptions configures bounded metadata retention and master-key rotation.
// KeyID identifies key; OldKeys only decrypt. IDs must not be reused for a new
// master key while snapshots encrypted with the previous key can still exist.
// Zero Retention selects five minutes; configure it above any command lifetime.
type RedisOptions struct {
	Retention time.Duration
	// IndexGrace leaves prepublished, uncommitted entries alone. Zero selects
	// five minutes; it must exceed the two-second transition command limit.
	IndexGrace time.Duration
	KeyID      byte
	OldKeys    map[byte][]byte
}

// ErrStateSuperseded means a delayed put was rejected by a newer snapshot.
// It does not revoke the writer's lease.
const transitionCommandLimit = 2 * time.Second

var errEpochOvertaken = errors.New("epoch allocation overtaken")

var ErrStateSuperseded = errors.New("sessionstore: snapshot superseded")

// TransitionResolver settles an uncertain candidate, fencing it if it did not
// commit. A false result is definitive; Get alone cannot prove non-commit.
type TransitionResolver interface {
	Settle(context.Context, Lease) (Lease, bool, error)
}

var _ Store = (*Redis)(nil)

// NewRedis creates a store using the repo's Redis connection configuration.
// key must be 32 bytes; nil reads RELAIS_SESSIONSTORE_KEY (standard base64).
// The store owns its client; Close releases connections, never stored sessions.
func NewRedis(ctx context.Context, cfg storage.RedisConfig, key []byte, options ...RedisOptions) (*Redis, error) {
	if strings.TrimSpace(cfg.Addr) == "" && len(cfg.Addrs) == 0 {
		return nil, errors.New("sessionstore: Redis address required")
	}
	for _, addr := range cfg.Addrs {
		if strings.TrimSpace(addr) == "" {
			return nil, errors.New("sessionstore: empty Redis cluster address")
		}
	}
	if key == nil {
		var err error
		key, err = base64.StdEncoding.DecodeString(os.Getenv("RELAIS_SESSIONSTORE_KEY"))
		if err != nil {
			return nil, fmt.Errorf("sessionstore: decode RELAIS_SESSIONSTORE_KEY: %w", err)
		}
	}
	if len(key) != 32 {
		return nil, errors.New("sessionstore: AES-256 key must contain 32 bytes")
	}
	// Braces in a prefix would override the per-session Cluster hash tag.
	if strings.ContainsAny(cfg.Prefix, "{}") {
		return nil, errors.New("sessionstore: prefix cannot contain braces")
	}
	opts := RedisOptions{Retention: 5 * time.Minute}
	if len(options) > 1 {
		return nil, errors.New("sessionstore: one RedisOptions required")
	}
	if len(options) == 1 {
		opts = options[0]
		if opts.Retention == 0 {
			opts.Retention = 5 * time.Minute
		}
	}
	if opts.IndexGrace == 0 {
		opts.IndexGrace = 5 * time.Minute
	}
	if opts.IndexGrace <= transitionCommandLimit {
		return nil, errors.New("sessionstore: index grace must exceed two-second command lifetime")
	}
	if opts.Retention < 0 {
		return nil, errors.New("sessionstore: positive retention required")
	}
	keysByID := map[byte][]byte{opts.KeyID: append([]byte(nil), key...)}
	for id, old := range opts.OldKeys {
		if id == opts.KeyID || len(old) != 32 {
			return nil, errors.New("sessionstore: old keys need unique IDs and 32 bytes")
		}
		keysByID[id] = append([]byte(nil), old...)
	}
	client := connectRedis(cfg)
	r := &Redis{client: client, prefix: cfg.Prefix, retention: opts.Retention, indexGrace: opts.IndexGrace, maintenanceSlots: make(chan struct{}, 32), activeKey: opts.KeyID, keysByID: keysByID}
	if err := r.retryRead(ctx, "session_ping", func() error { return client.Ping(ctx).Err() }); err != nil {
		_ = client.Close()
		return nil, err
	}
	return r, nil
}
func (r *Redis) Close() error {
	r.maintenanceMu.Lock()
	r.closed = true
	r.maintenanceMu.Unlock()
	r.maintenance.Wait()
	return r.client.Close()
}

// Best-effort repairs never delay callers or allocate an unbounded goroutine
// set. Close drains the at-most-32 jobs, each limited to two seconds.
func (r *Redis) maintain(fn func()) {
	r.maintenanceMu.Lock()
	defer r.maintenanceMu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.maintenanceSlots <- struct{}{}:
	default:
		return
	}
	r.maintenance.Add(1)
	go func() { defer r.maintenance.Done(); defer func() { <-r.maintenanceSlots }(); fn() }()
}

func (r *Redis) keys(id string) []string {
	// Encode unusual IDs containing braces so distinct IDs cannot select the
	// wrong tag or alias another session. Ordinary ICE ufrags keep storage's tag.
	tagID := id
	if strings.ContainsAny(id, "{}") || strings.HasPrefix(id, "~") {
		tagID = "~" + base64.RawURLEncoding.EncodeToString([]byte(id))
	}
	tag := "{sess:" + tagID + "}"
	return []string{r.prefix + "lease:" + tag, r.prefix + "state:" + tag, r.prefix + "record:" + tag, r.prefix + "epoch:" + tag, r.prefix + "routes:" + tag}
}

// All per-session transitions, including read/prune and fenced snapshot writes,
// happen in one script. Redis TIME makes TTL and ExpiresAt use the same clock.
// Epochs are compared as decimal strings (Lua numbers lose precision above 2^53).
const sessionLua = `
local op, owner, epoch, ttl = ARGV[1], ARGV[2], ARGV[3], tonumber(ARGV[4])
local retention = tonumber(ARGV[5])
local current = redis.call('HMGET', KEYS[1], 'worker', 'epoch', 'expires')
local function reply() return {1, current[1], current[2], current[3]} end
local function matches() return current[1] == owner and current[2] == epoch end
local function greater(a,b) return not b or #a>#b or (#a==#b and a>b) end
local function newer(candidate) return greater(candidate,redis.call('GET',KEYS[4])) end
local function now()
 local t=redis.call('TIME')
 return tonumber(t[1])*1000 + math.floor(tonumber(t[2])/1000)
end
local function retain(candidate)
 redis.call('SET',KEYS[4],candidate)
 redis.call('PEXPIREAT',KEYS[4],math.max(now(),tonumber(current[3]) or 0)+retention)
end
local function write(worker, nextEpoch)
 local expires=string.format('%.0f',now()+ttl)
 redis.call('HSET',KEYS[1],'worker',worker,'epoch',nextEpoch,'expires',expires)
 redis.call('PEXPIREAT',KEYS[1],expires)
 redis.call('PEXPIREAT',KEYS[2],expires)
 redis.call('SET',KEYS[3],worker .. '/' .. nextEpoch)
 redis.call('PEXPIREAT',KEYS[3],tonumber(expires)+retention)
 redis.call('PEXPIREAT',KEYS[4],tonumber(expires)+retention)
 current={worker,nextEpoch,expires}
 return reply()
end
if op=='settle' or op=='indexed' then
 if matches() then return reply() end
 -- A young prepublication may still commit. Never fence or prune it.
 if op=='indexed' and newer(epoch) and now()<tonumber(ARGV[6])+tonumber(ARGV[7]) then return {3} end
 if op=='indexed' and not current[1] and redis.call('GET',KEYS[3])==owner .. '/' .. epoch then
  redis.call('DEL',KEYS[2],KEYS[3],KEYS[5])
 end
 -- This also settles an indexed prepublication racing its transition. Once
 -- pruned, that candidate is fenced, never a live lease missing its index.
 if newer(epoch) then retain(epoch) end
 return {2}
elseif op=='claim' then
 if redis.call('EXISTS',KEYS[3])==1 then return {-1} end
 if not newer(epoch) then return {-3} end
 retain(epoch)
 redis.call('DEL',KEYS[2],KEYS[5])
 return write(owner,epoch)
elseif op=='get' or op=='state' or op=='checkpoint' then
 if not current[1] then
  redis.call('DEL',KEYS[2],KEYS[3],KEYS[5])
  return {0}
 end
 if op=='get' then return reply() end
 if op=='checkpoint' then
  local stored=redis.call('HGET',KEYS[1],'checkpoint_at')
  if not stored then return {0} end
  local digest=redis.call('HGET',KEYS[1],'state_digest')
  local matched=false
  for _,candidate in ipairs(cjson.decode(ARGV[6])) do
   if candidate==digest then matched=true; break end
  end
  if not matched then return {-4} end
  return {1,stored,string.format('%.0f',now())}
 end
 local blob=redis.call('GET',KEYS[2])
 if not blob then return {0} end
 return {1,blob,redis.call('HGET',KEYS[1],'state_seq') or ''}
elseif op=='release' then
 if matches() or (not current[1] and redis.call('GET',KEYS[3])==owner .. '/' .. epoch) then
  redis.call('DEL',KEYS[1],KEYS[2],KEYS[3],KEYS[5])
 end
 return {1}
end
if not matches() then return {-2} end
if op=='renew' then return write(owner,epoch)
elseif op=='transfer' then
 if not newer(ARGV[7]) then return {-3} end
 retain(ARGV[7])
 redis.call('DEL',KEYS[5])
 return write(ARGV[6],ARGV[7])
elseif op=='sequence' then
 redis.call('HINCRBY',KEYS[1],'next_seq',1)
 return {1,redis.call('HGET',KEYS[1],'next_seq')}
elseif op=='put' then
 local previous=redis.call('HGET',KEYS[1],'state_seq')
 if not greater(ARGV[7],previous) then return {-4} end
 redis.call('SET',KEYS[2],ARGV[6])
 redis.call('HSET',KEYS[1],'state_seq',ARGV[7],'checkpoint_at',string.format('%.0f',now()),'state_digest',ARGV[8])
 redis.call('PEXPIREAT',KEYS[2],current[3])
 return {1}
end
return redis.error_reply('unknown session operation')
`

var sessionScript = redis.NewScript(sessionLua)

func redisKind(err error) string {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "eof"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "timeout"
		}
		return "net"
	}
	upper := strings.ToUpper(err.Error())
	for _, kind := range []string{"moved", "ask", "tryagain", "busy", "clusterdown", "eof"} {
		if strings.HasPrefix(upper, strings.ToUpper(kind)) {
			return kind
		}
	}
	return "other"
}
func definiteNotRun(kind string) bool {
	return kind == "moved" || kind == "ask" || kind == "tryagain" || kind == "clusterdown" || kind == "busy"
}
func classify(op string, err error) error {
	if err == nil || errors.Is(err, redis.Nil) {
		return err
	}
	if redisKind(err) == "other" {
		return fmt.Errorf("sessionstore: %s: %w", op, err)
	}
	return &TransientError{Op: op, Err: err}
}
func retryable(kind string) bool { return kind != "other" && kind != "canceled" }

// Same capped three-attempt exponential backoff/jitter and Prometheus metrics
// as storage.withRetry, restricted to reads (no blind CAS or snapshot replay).
func (r *Redis) retryRead(ctx context.Context, op string, fn func() error) error {
	backoff := 20 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.observe(op, fn)
		if err == nil || errors.Is(err, redis.Nil) {
			return err
		}
		if ctx.Err() != nil || attempt == 2 || !retryable(redisKind(err)) {
			return classify(op, err)
		}
		metrics.RedisRetries.WithLabelValues(op).Inc()
		// Cryptographic randomness avoids shared RNG state with media/tests.
		var jitter [1]byte
		_, _ = rand.Read(jitter[:])
		timer := time.NewTimer(backoff + time.Duration(jitter[0])*backoff/512)
		select {
		case <-ctx.Done():
			timer.Stop()
			return classify(op, err)
		case <-timer.C:
		}
		backoff = min(2*backoff, 500*time.Millisecond)
	}
	return errors.New("sessionstore: unreachable retry")
}
func (r *Redis) observe(op string, fn func() error) error {
	start := time.Now()
	err := fn()
	metrics.RedisOpDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	if err != nil && !errors.Is(err, redis.Nil) {
		metrics.RedisErrors.WithLabelValues(op, redisKind(err)).Inc()
	}
	return err
}
func (r *Redis) run(ctx context.Context, op, id, owner, epoch string, ttl time.Duration, extras ...interface{}) ([]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args := []interface{}{op, owner, epoch, max(ttl.Milliseconds(), int64(1)), r.retention.Milliseconds()}
	args = append(args, extras...)
	var result []interface{}
	fn := func() error {
		var err error
		result, err = sessionScript.Run(ctx, r.client, r.keys(id), args...).Slice()
		return err
	}
	var err error
	if op == "get" || op == "state" || op == "settle" || op == "indexed" || op == "checkpoint" {
		err = r.retryRead(ctx, "session_"+op, fn)
	} else {
		for attempt := 0; attempt < 3; attempt++ {
			err = r.observe("session_"+op, fn)
			if err == nil || !definiteNotRun(redisKind(err)) || attempt == 2 {
				break
			}
			metrics.RedisRetries.WithLabelValues("session_" + op).Inc()
			timer := time.NewTimer(time.Duration(20<<attempt) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, classify(op, ctx.Err())
			case <-timer.C:
			}
		}
		err = classify(op, err)
	}
	if err != nil {
		return nil, err
	}
	switch result[0].(int64) {
	case 0:
		return nil, ErrNotFound
	case -1:
		return nil, ErrLeaseHeld
	case -2:
		return nil, ErrLeaseLost
	case -4:
		return nil, ErrStateSuperseded
	case -3:
		return nil, &TransientError{Op: op, Err: errEpochOvertaken}
	}
	return result, nil
}
func decodeLease(id string, result []interface{}) (Lease, error) {
	if len(result) < 4 {
		return Lease{}, errors.New("sessionstore: malformed lease")
	}
	workerText, workerOK := result[1].(string)
	epochText, epochOK := result[2].(string)
	expiresText, expiresOK := result[3].(string)
	if !workerOK || !epochOK || !expiresOK {
		return Lease{}, errors.New("sessionstore: malformed lease fields")
	}
	worker, err := netip.ParseAddrPort(workerText)
	if err != nil {
		return Lease{}, err
	}
	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil {
		return Lease{}, err
	}
	expires, err := strconv.ParseInt(expiresText, 10, 64)
	if err != nil {
		return Lease{}, err
	}
	return Lease{SessionID: id, Worker: worker, Epoch: epoch, ExpiresAt: time.UnixMilli(expires)}, nil
}
func (r *Redis) allocate(ctx context.Context) (string, error) {
	var epoch int64
	err := r.observe("session_epoch", func() error {
		var err error
		epoch, err = r.client.Incr(ctx, r.prefix+"session_epoch").Result()
		return err
	})
	if err != nil {
		return "", classify("epoch", err)
	}
	return strconv.FormatInt(epoch, 10), nil
}
func (r *Redis) indexKey(worker netip.AddrPort) string { return r.prefix + "worker:" + worker.String() }
func indexMember(lease Lease) string {
	raw, _ := json.Marshal(struct {
		ID    string
		Epoch uint64
	}{base64.RawURLEncoding.EncodeToString([]byte(lease.SessionID)), lease.Epoch})
	return string(raw)
}

// Publishing membership and its bounded lifetime is one idempotent operation:
// losing even the first reply cannot leave an immortal index key. ZADD NX
// preserves the original Redis-clock publication time during later repairs.
const indexLua = `local t=redis.call('TIME'); redis.call('ZADD',KEYS[1],'NX',tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000),ARGV[1]); local remaining=redis.call('PTTL',KEYS[1]); if remaining<tonumber(ARGV[2]) then redis.call('PEXPIRE',KEYS[1],ARGV[2]) end; return 1`

func (r *Redis) indexContext(ctx context.Context, lease Lease, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	return r.retryRead(ctx, "session_index_add", func() error {
		return r.client.Eval(ctx, indexLua, []string{r.indexKey(lease.Worker)}, indexMember(lease), max(ttl+r.retention, r.indexGrace+r.retention).Milliseconds()).Err()
	})
}
func (r *Redis) index(lease Lease, ttl time.Duration) error {
	return r.indexContext(context.Background(), lease, ttl)
}
func (r *Redis) unindex(lease Lease) {
	r.maintain(func() {
		ctx, cancel := context.WithTimeout(context.Background(), transitionCommandLimit)
		defer cancel()
		_ = r.retryRead(ctx, "session_index_prune", func() error { return r.client.ZRem(ctx, r.indexKey(lease.Worker), indexMember(lease)).Err() })
	})
}
func (r *Redis) repairIndex(lease Lease, ttl time.Duration) {
	r.maintain(func() { _ = r.index(lease, ttl) })
}

// Refresh once per worker tick without the droppable maintenance queue.
// Extending only avoids shortening an index with a longer-lived member.
var indexRefreshScript = redis.NewScript(`local remaining=redis.call('PTTL',KEYS[1]); if remaining>=0 and remaining<tonumber(ARGV[1]) then return redis.call('PEXPIRE',KEYS[1],ARGV[1]) end; return 0`)

func (r *Redis) RefreshWorkerIndex(ctx context.Context, worker netip.AddrPort, ttl time.Duration) error {
	if !worker.IsValid() || ttl <= 0 {
		return errors.New("sessionstore: index refresh needs a valid worker and positive TTL")
	}
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	return r.retryRead(ctx, "session_index_refresh", func() error {
		return indexRefreshScript.Run(ctx, r.client, []string{r.indexKey(worker)}, (max(ttl, r.indexGrace) + r.retention).Milliseconds()).Err()
	})
}

// Settle is idempotent. A negative answer fences even a still-in-flight command.
func (r *Redis) Settle(ctx context.Context, candidate Lease) (Lease, bool, error) {
	result, err := r.run(ctx, "settle", candidate.SessionID, candidate.Worker.String(), strconv.FormatUint(candidate.Epoch, 10), 0)
	if err != nil {
		return Lease{}, false, err
	}
	if result[0].(int64) == 2 {
		return Lease{}, false, nil
	}
	lease, err := decodeLease(candidate.SessionID, result)
	return lease, err == nil, err
}
func (r *Redis) recoverTransition(candidate Lease, op string, cause error) (Lease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, committed, err := r.Settle(ctx, candidate)
	if err != nil {
		return Lease{}, &TransientError{Op: op, Err: errors.Join(cause, err), Candidate: &candidate}
	}
	if committed {
		return lease, nil
	}
	r.unindex(candidate)
	return Lease{}, &TransientError{Op: op, Err: cause}
}
func candidateLease(id, epoch string, worker netip.AddrPort) Lease {
	n, _ := strconv.ParseUint(epoch, 10, 64)
	return Lease{SessionID: id, Worker: worker, Epoch: n}
}
func (r *Redis) Claim(ctx context.Context, id string, worker netip.AddrPort, ttl time.Duration) (Lease, error) {
	var lease Lease
	var err error
	for range 3 {
		lease, err = r.claim(ctx, id, worker, ttl)
		if !errors.Is(err, errEpochOvertaken) {
			return lease, err
		}
	}
	return lease, err
}
func (r *Redis) claim(ctx context.Context, id string, worker netip.AddrPort, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if !valid(id, worker, ttl) {
		return Lease{}, errors.New("sessionstore: claim needs a session ID, address and positive TTL")
	}
	epoch, err := r.allocate(ctx)
	if err != nil {
		return Lease{}, err
	}
	candidate := candidateLease(id, epoch, worker)
	if err := r.indexContext(ctx, candidate, ttl); err != nil {
		return Lease{}, err
	}
	result, err := r.run(ctx, "claim", id, worker.String(), epoch, ttl)
	if errors.Is(err, errEpochOvertaken) {
		r.unindex(candidate)
		return Lease{}, err
	}
	if errors.Is(err, ErrTransient) {
		return r.recoverTransition(candidate, "claim", err)
	}
	if err != nil {
		r.unindex(candidate)
		return Lease{}, err
	}
	lease, err := decodeLease(id, result)
	if err == nil {
		r.repairIndex(lease, ttl)
	}
	return lease, err
}
func (r *Redis) Get(ctx context.Context, id string) (Lease, error) {
	result, err := r.run(ctx, "get", id, "", "", 0)
	if err != nil {
		return Lease{}, err
	}
	return decodeLease(id, result)
}
func (r *Redis) Owner(ctx context.Context, id string) (netip.AddrPort, error) {
	lease, err := r.Get(ctx, id)
	return lease.Worker, err
}
func (r *Redis) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if ttl <= 0 {
		return Lease{}, errors.New("sessionstore: positive TTL required")
	}
	result, err := r.run(ctx, "renew", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), ttl)
	if err != nil {
		return Lease{}, err
	}
	renewed, err := decodeLease(lease.SessionID, result)
	if err == nil {
		r.repairIndex(renewed, ttl)
	}
	return renewed, err
}
func (r *Redis) Transfer(ctx context.Context, from Lease, to netip.AddrPort, ttl time.Duration) (Lease, error) {
	var lease Lease
	var err error
	for range 3 {
		lease, err = r.transfer(ctx, from, to, ttl)
		if !errors.Is(err, errEpochOvertaken) {
			return lease, err
		}
	}
	return lease, err
}
func (r *Redis) transfer(ctx context.Context, from Lease, to netip.AddrPort, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if !valid(from.SessionID, to, ttl) || from.Worker == to {
		return Lease{}, errors.New("sessionstore: transfer needs a different valid worker and positive TTL")
	}
	epoch, err := r.allocate(ctx)
	if err != nil {
		return Lease{}, err
	}
	candidate := candidateLease(from.SessionID, epoch, to)
	if err := r.indexContext(ctx, candidate, ttl); err != nil {
		return Lease{}, err
	}
	result, err := r.run(ctx, "transfer", from.SessionID, from.Worker.String(), strconv.FormatUint(from.Epoch, 10), ttl, to.String(), epoch)
	if errors.Is(err, errEpochOvertaken) {
		r.unindex(candidate)
		return Lease{}, err
	}
	if errors.Is(err, ErrTransient) {
		lease, recoverErr := r.recoverTransition(candidate, "transfer", err)
		if recoverErr == nil {
			r.unindex(from)
		}
		return lease, recoverErr
	}
	if err != nil {
		r.unindex(candidate)
		return Lease{}, err
	}
	lease, err := decodeLease(from.SessionID, result)
	if err == nil {
		r.unindex(from)
		r.repairIndex(lease, ttl)
	}
	return lease, err
}
func (r *Redis) Release(ctx context.Context, lease Lease) error {
	_, err := r.run(ctx, "release", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 0)
	if err == nil {
		r.unindex(lease)
	}
	return err
}
func (r *Redis) aead(id string, keyID byte) (cipher.AEAD, error) {
	master, ok := r.keysByID[keyID]
	if !ok {
		return nil, errors.New("sessionstore: unknown snapshot key ID")
	}
	key, err := hkdf.Key(sha256.New, master, nil, "relais/sessionstore/v1/"+id, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Opaque IDs and binary headers must survive JSON encoding without UTF-8 loss.
func stateAAD(id, seq string, header []byte) []byte {
	data, _ := json.Marshal([]string{base64.RawURLEncoding.EncodeToString([]byte(id)), seq, base64.StdEncoding.EncodeToString(header)})
	return data
}
func (r *Redis) PutState(ctx context.Context, lease Lease, state []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := r.run(ctx, "sequence", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 0)
	if err != nil {
		return err
	}
	seq := result[1].(string)
	seal, err := r.aead(lease.SessionID, r.activeKey)
	if err != nil {
		return err
	}
	header := []byte{1, r.activeKey}
	nonce := make([]byte, seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	blob := append(header, nonce...)
	blob = seal.Seal(blob, nonce, state, stateAAD(lease.SessionID, seq, header))
	_, err = r.run(ctx, "put", lease.SessionID, lease.Worker.String(), strconv.FormatUint(lease.Epoch, 10), 0, blob, seq, r.stateDigest(lease.SessionID, r.activeKey, state))
	return err
}
func (r *Redis) GetState(ctx context.Context, id string) ([]byte, error) {
	result, err := r.run(ctx, "state", id, "", "", 0)
	if err != nil {
		return nil, err
	}
	blob := []byte(result[1].(string))
	seq := result[2].(string)
	if len(blob) < 2 || blob[0] != 1 {
		return nil, errors.New("sessionstore: invalid snapshot format")
	}
	seal, err := r.aead(id, blob[1])
	if err != nil {
		return nil, err
	}
	n := seal.NonceSize()
	if len(blob) < 2+n {
		return nil, errors.New("sessionstore: truncated encrypted state")
	}
	state, err := seal.Open(nil, blob[2:2+n], blob[2+n:], stateAAD(id, seq, blob[:2]))
	if err != nil {
		return nil, fmt.Errorf("sessionstore: authenticate state: %w", err)
	}
	return state, nil
}
func (r *Redis) ListByWorker(ctx context.Context, worker netip.AddrPort) ([]Lease, error) {
	var members []redis.Z
	err := r.retryRead(ctx, "session_index_list", func() error {
		var err error
		members, err = r.client.ZRangeWithScores(ctx, r.indexKey(worker), 0, -1).Result()
		return err
	})
	if err != nil {
		return nil, err
	}
	type check struct {
		member    string
		id        string
		epoch     uint64
		published int64
		cmd       *redis.Cmd
	}
	checks := make([]check, 0, len(members))
	stale := make([]interface{}, 0)
	for _, indexed := range members {
		member := indexed.Member.(string)
		var item struct {
			ID    string
			Epoch uint64
		}
		if err := json.Unmarshal([]byte(member), &item); err != nil || item.ID == "" || item.Epoch == 0 {
			stale = append(stale, member)
			continue
		}
		id, err := base64.RawURLEncoding.DecodeString(item.ID)
		if err != nil {
			stale = append(stale, member)
			continue
		}
		checks = append(checks, check{member: member, id: string(id), epoch: item.Epoch, published: int64(indexed.Score)})
	}
	// EVALSHA sends only the digest per member. On a cold/restarted node, load
	// once on that member's master and retry the idempotent pipeline.
	err = r.retryRead(ctx, "session_index_check", func() error {
		pipe := r.client.Pipeline()
		defer func() { _ = pipe.Close() }()
		for i := range checks {
			c := &checks[i]
			c.cmd = pipe.EvalSha(ctx, sessionScript.Hash(), r.keys(c.id), "indexed", worker.String(), strconv.FormatUint(c.epoch, 10), 1, r.retention.Milliseconds(), c.published, r.indexGrace.Milliseconds())
		}
		_, execErr := pipe.Exec(ctx)
		if execErr != nil && redisKind(execErr) != "other" {
			return execErr
		}
		// Reload each affected node, then retry with EVALSHA.
		loaded := map[string]bool{}
		for _, c := range checks {
			if e := c.cmd.Err(); e != nil && strings.HasPrefix(e.Error(), "NOSCRIPT ") {
				node := r.client
				if cluster, ok := r.client.(*redis.ClusterClient); ok {
					master, e := cluster.MasterForKey(ctx, r.keys(c.id)[0])
					if e != nil {
						return e
					}
					node = master
				}
				address := fmt.Sprintf("%p", node)
				if !loaded[address] {
					if e := node.ScriptLoad(ctx, sessionLua).Err(); e != nil {
						return e
					}
					loaded[address] = true
				}
			}
		}
		if len(loaded) > 0 {
			return errors.New("TRYAGAIN scripts loaded")
		}
		// Individual corrupt records do not hide unrelated healthy members.
		for _, c := range checks {
			if e := c.cmd.Err(); e != nil && !strings.HasPrefix(e.Error(), "WRONGTYPE ") {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	leases := make([]Lease, 0, len(checks))
	for _, c := range checks {
		result, e := c.cmd.Slice()
		if e != nil {
			continue
		} // keep unreadable records; never prune uncertain membership
		if result[0].(int64) == 3 {
			continue
		}
		if result[0].(int64) != 1 {
			stale = append(stale, c.member)
			continue
		}
		lease, e := decodeLease(c.id, result)
		if e != nil {
			continue
		}
		leases = append(leases, lease)
	}
	if len(stale) > 0 {
		_ = r.observe("session_index_prune", func() error { return r.client.ZRem(ctx, r.indexKey(worker), stale...).Err() })
	}
	return leases, nil
}

func connectRedis(cfg storage.RedisConfig) redis.UniversalClient {
	var client redis.UniversalClient
	if cfg.Cluster || len(cfg.Addrs) > 0 {
		addrs := cfg.Addrs
		if len(addrs) == 0 {
			addrs = []string{cfg.Addr}
		}
		client = redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs, Password: cfg.Password, MaxRetries: -1, MaxRedirects: -1})
	} else {
		client = redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB, MaxRetries: -1})
	}
	return client
}

// Clock samples Redis TIME before a media copy, so a delayed PutState cannot
// make old counters look fresh merely because the write eventually succeeds.
func (r *Redis) Clock(ctx context.Context, id string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, transitionCommandLimit)
	defer cancel()
	var result []interface{}
	err := r.retryRead(ctx, "session_clock", func() error {
		var err error
		result, err = r.client.Eval(ctx, `return redis.call('TIME')`, r.keys(id)).Slice()
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	if len(result) != 2 {
		return time.Time{}, errors.New("sessionstore: invalid clock response")
	}
	seconds, err := strconv.ParseInt(fmt.Sprint(result[0]), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	micros, err := strconv.ParseInt(fmt.Sprint(result[1]), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(seconds, micros*1000).Truncate(time.Millisecond), nil
}

// Domain separation keeps the metadata authenticator independent of the AES
// encryption key. Include session ID and key ID to prevent cross-record reuse.
func (r *Redis) stateDigest(id string, keyID byte, state []byte) string {
	mac := hmac.New(sha256.New, r.keysByID[keyID])
	mac.Write(stateAAD(id, "checkpoint-digest-v1", []byte{keyID}))
	mac.Write(state)
	return fmt.Sprintf("%d:%x", keyID, mac.Sum(nil))
}

func (r *Redis) Checkpoint(ctx context.Context, id string, state []byte) (Checkpoint, error) {
	digests := make([]string, 0, len(r.keysByID))
	for keyID := range r.keysByID {
		digests = append(digests, r.stateDigest(id, keyID, state))
	}
	candidates, err := json.Marshal(digests)
	if err != nil {
		return Checkpoint{}, err
	}
	result, err := r.run(ctx, "checkpoint", id, "", "", 0, string(candidates))
	if err != nil {
		return Checkpoint{}, err
	}
	stored, err := strconv.ParseInt(result[1].(string), 10, 64)
	if err != nil {
		return Checkpoint{}, err
	}
	now, err := strconv.ParseInt(result[2].(string), 10, 64)
	if err != nil {
		return Checkpoint{}, err
	}
	if now < stored {
		return Checkpoint{}, ErrUnsafeCheckpointClock
	}
	return Checkpoint{StoredAt: time.UnixMilli(stored), Now: time.UnixMilli(now), Age: time.Duration(max(now-stored, 0)) * time.Millisecond}, nil
}
