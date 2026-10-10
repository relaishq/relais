package framecache

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/storage"
)

// Redis retains one current group per track in a single per-session hash. One
// key makes append, read, expiry refresh and session deletion atomic, including
// on Redis Cluster. Payload bytes/frame caps match Memory; global Memory LRU
// caps do not apply. Every hash expires after IdleTTL (30s by default).
// The caller owns Close. An uncertain append is never retried.
type Redis struct {
	client    redis.UniversalClient
	prefix    string
	limits    Limits
	activeKey byte
	keysByID  map[byte][]byte
}

// RedisOptions permits rotation using the same master key as sessionstore.
// OldKeys decrypt only; never reuse an ID while old ciphertext can remain.
type RedisOptions struct {
	KeyID   byte
	OldKeys map[byte][]byte
}

var _ Store = (*Redis)(nil)

func NewRedis(ctx context.Context, cfg storage.RedisConfig, key []byte, limits Limits, options ...RedisOptions) (*Redis, error) {
	if strings.TrimSpace(cfg.Addr) == "" && len(cfg.Addrs) == 0 {
		return nil, errors.New("framecache: explicit Redis address required")
	}
	for _, addr := range cfg.Addrs {
		if strings.TrimSpace(addr) == "" {
			return nil, errors.New("framecache: empty cluster address")
		}
	}
	if strings.ContainsAny(cfg.Prefix, "{}") {
		return nil, errors.New("framecache: prefix cannot contain braces")
	}
	if key == nil {
		var err error
		key, err = base64.StdEncoding.DecodeString(os.Getenv("RELAIS_SESSIONSTORE_KEY"))
		if err != nil {
			return nil, fmt.Errorf("framecache: decode store key: %w", err)
		}
	}
	if len(key) != 32 {
		return nil, errors.New("framecache: AES-256 key must contain 32 bytes")
	}
	if len(options) > 1 {
		return nil, errors.New("framecache: at most one RedisOptions")
	}
	opts := RedisOptions{}
	if len(options) == 1 {
		opts = options[0]
	}
	keys := map[byte][]byte{opts.KeyID: append([]byte(nil), key...)}
	for id, old := range opts.OldKeys {
		if id == opts.KeyID || len(old) != 32 {
			return nil, errors.New("framecache: old keys require unique IDs and 32 bytes")
		}
		keys[id] = append([]byte(nil), old...)
	}
	var client redis.UniversalClient
	// Never inherit storage's localhost:6379 default. Disable network retries:
	// a lost append reply must not duplicate a frame or clear the current group.
	if cfg.Cluster || len(cfg.Addrs) > 0 {
		addrs := cfg.Addrs
		if len(addrs) == 0 {
			addrs = []string{cfg.Addr}
		}
		client = redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs, Password: cfg.Password, MaxRetries: -1, MaxRedirects: -1})
	} else {
		client = redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB, MaxRetries: -1})
	}
	r := &Redis{client: client, prefix: cfg.Prefix, limits: defaultLimits(limits), activeKey: opts.KeyID, keysByID: keys}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return r, nil
}
func (r *Redis) Close() error { return r.client.Close() }
func (r *Redis) key(id string) string {
	tag := id
	if strings.ContainsAny(id, "{}") || strings.HasPrefix(id, "~") {
		tag = "~" + hex.EncodeToString([]byte(id))
	}
	return r.prefix + "frames:{sess:" + tag + "}"
}
func trackField(t Track) string {
	return hex.EncodeToString([]byte(t.Kind)) + ":" + strconv.FormatUint(uint64(t.SSRC), 10) + ":"
}

// Fields b/n/s/t hold payload bytes, count, last sequence and RTP timestamp.
// f1..fn hold versioned encrypted binary records. All changes share one script;
// readers never see partial replacement/overflow or mixed keyframe groups.
const appendLua = `
local p, size, cap, maxframes, ttl = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5])
local n=tonumber(redis.call('HGET',KEYS[1],p..'n')) or 0
local function clear()
 for i=1,n do redis.call('HDEL',KEYS[1],p..'f'..i) end
 redis.call('HDEL',KEYS[1],p..'b',p..'n',p..'s',p..'t')
 n=0
end
local function finish() redis.call('PEXPIRE',KEYS[1],ttl); return 1 end
if ARGV[6]=='1' then clear()
else
 if n==0 then return finish() end
 local seq=tonumber(redis.call('HGET',KEYS[1],p..'s'))
 local ts=tonumber(redis.call('HGET',KEYS[1],p..'t'))
 local delta=(tonumber(ARGV[8])-ts)%4294967296
 if tonumber(ARGV[7])~=(seq+1)%65536 or delta==0 or delta>=2147483648 then clear(); return finish() end
end
local bytes=tonumber(redis.call('HGET',KEYS[1],p..'b')) or 0
if bytes+size>cap or n+1>maxframes then clear(); return finish() end
redis.call('HSET',KEYS[1],p..'f'..(n+1),ARGV[10],p..'n',n+1,p..'b',bytes+size,p..'s',ARGV[9],p..'t',ARGV[8])
return finish()
`
const currentLua = `
local p=ARGV[1]
local n=tonumber(redis.call('HGET',KEYS[1],p..'n')) or 0
if n>tonumber(ARGV[3]) then return redis.error_reply('framecache: frame cap exceeded') end
local frames={}
for i=1,n do
 local f=redis.call('HGET',KEYS[1],p..'f'..i)
 if not f then return redis.error_reply('framecache: missing record') end
 frames[i]=f
end
redis.call('PEXPIRE',KEYS[1],ARGV[2])
return frames
`

var appendScript = redis.NewScript(appendLua)
var currentScript = redis.NewScript(currentLua)

func (r *Redis) aead(id string, keyID byte) (cipher.AEAD, error) {
	master, ok := r.keysByID[keyID]
	if !ok {
		return nil, errors.New("framecache: unknown key ID")
	}
	key, err := hkdf.Key(sha256.New, master, nil, "relais/framecache/v1/"+id, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func frameAAD(id string, track Track, header []byte) []byte {
	raw := binary.BigEndian.AppendUint32(nil, uint32(len(id)))
	raw = append(raw, id...)
	raw = binary.BigEndian.AppendUint32(raw, uint32(len(track.Kind)))
	raw = append(raw, track.Kind...)
	raw = binary.BigEndian.AppendUint32(raw, track.SSRC)
	return append(raw, header...)
}
func (r *Redis) seal(id string, f Frame) ([]byte, error) {
	plain, err := encodeFrame(f)
	if err != nil {
		return nil, err
	}
	aead, err := r.aead(id, r.activeKey)
	if err != nil {
		return nil, err
	}
	header := []byte{1, r.activeKey}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(append(header, nonce...), nonce, plain, frameAAD(id, f.Track, header)), nil
}
func (r *Redis) open(id string, track Track, raw []byte) (Frame, error) {
	if len(raw) < 2 || raw[0] != 1 {
		return Frame{}, errEncoding
	}
	aead, err := r.aead(id, raw[1])
	if err != nil {
		return Frame{}, err
	}
	n := aead.NonceSize()
	if len(raw) < 2+n+aead.Overhead() {
		return Frame{}, errEncoding
	}
	plain, err := aead.Open(nil, raw[2:2+n], raw[2+n:], frameAAD(id, track, raw[:2]))
	if err != nil {
		return Frame{}, fmt.Errorf("framecache: authenticate frame: %w", err)
	}
	f, err := decodeFrame(plain)
	if err != nil {
		return Frame{}, err
	}
	if f.Track != track {
		return Frame{}, errEncoding
	}
	return f, nil
}
func (r *Redis) Append(ctx context.Context, id string, f Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	size, err := frameSize(f)
	if err != nil {
		return err
	}
	var blob []byte
	if size <= r.limits.Bytes {
		blob, err = r.seal(id, f)
		if err != nil {
			return err
		}
	} // oversize still atomically clears the group, without encrypting a rejected payload
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return appendScript.Run(ctx, r.client, []string{r.key(id)}, trackField(f.Track), size, r.limits.Bytes, r.limits.Frames, max(r.limits.IdleTTL.Milliseconds(), int64(1)), boolByte(f.Keyframe), f.Packets[0].SequenceNumber, f.Timestamp, f.Packets[len(f.Packets)-1].SequenceNumber, blob).Err()
}
func (r *Redis) Current(ctx context.Context, id string, track Track) ([]Frame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := currentScript.Run(ctx, r.client, []string{r.key(id)}, trackField(track), max(r.limits.IdleTTL.Milliseconds(), int64(1)), r.limits.Frames).StringSlice()
	if err != nil {
		// go-redis may report a socket timeout just before the context timer fires.
		// Preserve its cause while giving takeover the correct cache-timeout reason.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(ctxErr, err)
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return nil, errors.Join(context.DeadlineExceeded, err)
		}
		return nil, err
	}
	frames := make([]Frame, len(raw))
	size := 0
	for i, blob := range raw {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frames[i], err = r.open(id, track, []byte(blob))
		if err != nil {
			return nil, err
		} // never expose a partial authenticated group
		n, err := frameSize(frames[i])
		if err != nil {
			return nil, err
		}
		size += n
		if size > r.limits.Bytes || (i == 0 && !frames[i].Keyframe) {
			return nil, errEncoding
		}
		if i > 0 {
			prev, f := frames[i-1], frames[i]
			if f.Keyframe || f.Packets[0].SequenceNumber != prev.Packets[len(prev.Packets)-1].SequenceNumber+1 || int32(f.Timestamp-prev.Timestamp) <= 0 {
				return nil, errEncoding
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return frames, nil
}
func (r *Redis) DeleteSession(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.client.Del(ctx, r.key(id)).Err()
}
