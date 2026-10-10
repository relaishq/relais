package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Route metadata is deliberately separate from encrypted media snapshots.
// The relay has no media key. Every route hash shares its session's lease tag;
// generation checks, re-nomination and hangup are atomic on Redis Cluster too.
type storedRoute struct {
	Generation    string `json:"generation"`
	Confirmed     string `json:"confirmed"`
	Authenticated string `json:"authenticated"`
	Expires       int64  `json:"expires"`
	Data          Route  `json:"data"`
}

func routeJSON(route Route) (string, error) {
	raw, err := json.Marshal(storedRoute{strconv.FormatUint(route.Generation, 10), strconv.FormatInt(route.ConfirmedAt.UnixNano(), 10), strconv.FormatInt(route.LastAuthenticated.UnixNano(), 10), route.ExpiresAt.UnixMilli(), route})
	return string(raw), err
}

const putRouteLua = `
local epoch=redis.call('HGET',KEYS[1],'epoch')
if epoch~=ARGV[1] then return -1 end
local t=redis.call('TIME'); local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local incoming=cjson.decode(ARGV[3])
if incoming.expires<=now then return 0 end
local function greater(a,b) return #a>#b or (#a==#b and a>b) end
local marker=redis.call('HGET',KEYS[2],'@nomination')
if marker and greater(marker,incoming.confirmed) then return 1 end
local old=redis.call('HGET',KEYS[2],ARGV[2])
if old then
 old=cjson.decode(old)
 if old.generation==incoming.generation and (greater(old.confirmed,incoming.confirmed) or (old.confirmed==incoming.confirmed and greater(old.authenticated,incoming.authenticated))) then return 1 end
end
if ARGV[4]=='1' then
 -- A delayed nomination must not delete a more recently confirmed address.
 local values=redis.call('HGETALL',KEYS[2])
 for i=1,#values,2 do
  if values[i]~='@nomination' then
  local previous=cjson.decode(values[i+1])
  if previous.generation==incoming.generation and greater(previous.confirmed,incoming.confirmed) then return 1 end
  end
 end
 redis.call('DEL',KEYS[2])
 redis.call('HSET',KEYS[2],'@nomination',incoming.confirmed)
end
redis.call('HSET',KEYS[2],ARGV[2],ARGV[3])
local deadline=incoming.expires+tonumber(ARGV[5])
local remaining=redis.call('PTTL',KEYS[2])
if remaining<deadline-now then redis.call('PEXPIREAT',KEYS[2],deadline) end
return 1
`

func (r *Redis) PutRoute(ctx context.Context, route Route, nominated bool) error {
	if !validRoute(route) {
		return errors.New("sessionstore: invalid route")
	}
	raw, err := routeJSON(route)
	if err != nil {
		return err
	}
	keys := r.keys(route.SessionID)
	replace := "0"
	if nominated {
		replace = "1"
	}
	n, err := r.client.Eval(ctx, putRouteLua, []string{keys[0], keys[4]}, strconv.FormatUint(route.Generation, 10), route.Caller.String(), raw, replace, RouteRetention.Milliseconds()).Int()
	if err != nil {
		return &TransientError{Op: "put_route", Err: err}
	}
	if n == -1 {
		return ErrLeaseLost
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

const deleteRouteLua = `if redis.call('HGET',KEYS[1],ARGV[1])==ARGV[2] then redis.call('HDEL',KEYS[1],ARGV[1]) end; return 1`

func (r *Redis) DeleteRoute(ctx context.Context, route Route) error {
	raw, err := routeJSON(route)
	if err != nil {
		return err
	}
	return r.client.Eval(ctx, deleteRouteLua, []string{r.keys(route.SessionID)[4]}, route.Caller.String(), raw).Err()
}
func (r *Redis) ForgetRoutes(ctx context.Context, id string) error {
	return r.client.Del(ctx, r.keys(id)[4]).Err()
}

// LoadRoutes scans only on startup. The shared limit also bounds cluster scans.
// SCAN duplicates are removed; no caller packet can trigger a scan or a lookup.
func (r *Redis) LoadRoutes(ctx context.Context, limit int) ([]Route, error) {
	if limit <= 0 {
		return nil, ErrRouteLimit
	}
	var mu sync.Mutex
	routes := []Route{}
	seen := make(map[string]bool)
	scan := func(ctx context.Context, client *redis.Client) error {
		mu.Lock()
		defer mu.Unlock()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, escapeRoutePattern(r.prefix)+"routes:*", 128).Result()
			if err != nil {
				return err
			}
			for _, key := range keys {
				if seen[key] {
					continue
				}
				seen[key] = true
				// Bound scanned session hashes as well as live records. An operator must
				// remove excessive stale metadata rather than silently skip protection.
				if len(seen) > limit {
					return ErrRouteLimit
				}
				n, err := r.client.HLen(ctx, key).Result()
				if err != nil {
					return err
				}
				if n > int64(limit-len(routes)+1) {
					return ErrRouteLimit
				}
				values, err := r.client.HGetAll(ctx, key).Result()
				if err != nil {
					return err
				}
				for caller, raw := range values {
					if caller == "@nomination" {
						continue
					}
					if len(routes) >= limit {
						return ErrRouteLimit
					}
					var record storedRoute
					if err := json.Unmarshal([]byte(raw), &record); err != nil {
						return fmt.Errorf("sessionstore: decode route: %w", err)
					}
					route := record.Data
					if !validRoute(route) || route.Caller.String() != caller || r.keys(route.SessionID)[4] != key {
						return errors.New("sessionstore: corrupt route metadata")
					}
					lease, err := r.Get(ctx, route.SessionID)
					if err != nil && !errors.Is(err, ErrNotFound) {
						return err
					}
					if errors.Is(err, ErrNotFound) || lease.Epoch != route.Generation || !time.Now().Before(route.ExpiresAt) {
						if err := r.DeleteRoute(ctx, route); err != nil {
							return err
						}
						continue
					}
					routes = append(routes, route)
				}
			}
			cursor = next
			if cursor == 0 {
				return nil
			}
		}
	}
	var err error
	switch client := r.client.(type) {
	case *redis.ClusterClient:
		err = client.ForEachMaster(ctx, scan)
	case *redis.Client:
		err = scan(ctx, client)
	default:
		err = errors.New("sessionstore: route scan needs a Redis client or cluster")
	}
	return routes, err
}

func escapeRoutePattern(prefix string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]").Replace(prefix)
}

func (r *RedisOwners) Get(ctx context.Context, id string) (Lease, error) {
	return r.leases.Get(ctx, id)
}
func (r *RedisOwners) PutRoute(ctx context.Context, route Route, nominated bool) error {
	return r.leases.PutRoute(ctx, route, nominated)
}
func (r *RedisOwners) LoadRoutes(ctx context.Context, limit int) ([]Route, error) {
	return r.leases.LoadRoutes(ctx, limit)
}
func (r *RedisOwners) DeleteRoute(ctx context.Context, route Route) error {
	return r.leases.DeleteRoute(ctx, route)
}
func (r *RedisOwners) ForgetRoutes(ctx context.Context, id string) error {
	return r.leases.ForgetRoutes(ctx, id)
}

var _ Routes = (*Memory)(nil)
var _ Routes = (*Redis)(nil)
var _ Routes = (*RedisOwners)(nil)
