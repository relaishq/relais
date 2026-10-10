package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
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
-- Prune old fields even when another address keeps the hash alive.
local values=redis.call('HGETALL',KEYS[2])
for i=1,#values,2 do
 if values[i]~='@nomination' then
  local ok,previous=pcall(cjson.decode,values[i+1])
  if not ok or previous.expires<=now then redis.call('HDEL',KEYS[2],values[i]) end
 end
end
local marker=redis.call('HGET',KEYS[2],'@nomination')
if marker and greater(marker,incoming.confirmed) then return 1 end
local old=redis.call('HGET',KEYS[2],ARGV[2])
if old then
 old=cjson.decode(old)
 if old.generation==incoming.generation and (greater(old.confirmed,incoming.confirmed) or (old.confirmed==incoming.confirmed and greater(old.authenticated,incoming.authenticated))) then return 1 end
end
if ARGV[4]=='1' and (not old or old.generation~=incoming.generation) then
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
-- Keep at most eight addresses, evicting the oldest authenticated evidence.
local values=redis.call('HGETALL',KEYS[2]); local entries={}
for i=1,#values,2 do
 if values[i]~='@nomination' then table.insert(entries,{values[i],cjson.decode(values[i+1])}) end
end
table.sort(entries,function(a,b)
 if a[2].authenticated~=b[2].authenticated then return greater(b[2].authenticated,a[2].authenticated) end
 return greater(b[2].confirmed,a[2].confirmed)
end)
for i=1,#entries-tonumber(ARGV[6]) do redis.call('HDEL',KEYS[2],entries[i][1]) end
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
	n, err := r.client.Eval(ctx, putRouteLua, []string{keys[0], keys[4]}, strconv.FormatUint(route.Generation, 10), route.Caller.String(), raw, replace, RouteRetention.Milliseconds(), MaxRoutesPerSession).Int()
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

// LoadRoutes scans only on startup and returns usable partial results on error.
func (r *Redis) LoadRoutes(ctx context.Context, limit int) ([]Route, error) {
	routes, _, err := r.LoadRouteOwners(ctx, limit)
	return routes, err
}

// LoadRouteOwners pipelines hash and lease reads per SCAN page on each master.
// Expired fields never consume the route limit or require a lease read.
func (r *Redis) LoadRouteOwners(ctx context.Context, limit int) ([]Route, map[string]Lease, error) {
	if limit <= 0 {
		return nil, nil, ErrRouteLimit
	}
	var mu sync.Mutex
	routes := []Route{}
	owners := make(map[string]Lease)
	seen := make(map[string]bool)
	scan := func(ctx context.Context, client *redis.Client) error {
		var cursor uint64
		var failures error
		for {
			keys, next, err := client.Scan(ctx, cursor, escapeRoutePattern(r.prefix)+"routes:*", 128).Result()
			if err != nil {
				return errors.Join(failures, err)
			}
			reads := make(map[string]*redis.StringStringMapCmd)
			pipe := client.Pipeline()
			for _, key := range keys {
				mu.Lock()
				duplicate := seen[key]
				seen[key] = true
				mu.Unlock()
				if !duplicate {
					reads[key] = pipe.HGetAll(ctx, key)
				}
			}
			_, readErr := pipe.Exec(ctx)
			failures = errors.Join(failures, readErr)
			candidates := make(map[string][]Route)
			cleanup := client.Pipeline()
			for key, cmd := range reads {
				values, err := cmd.Result()
				if err != nil {
					continue
				}
				for caller, raw := range values {
					if caller == "@nomination" {
						continue
					}
					var record storedRoute
					if err := json.Unmarshal([]byte(raw), &record); err != nil {
						failures = errors.Join(failures, err)
						continue
					}
					route := record.Data
					if !time.Now().Before(route.ExpiresAt) {
						cleanup.Eval(ctx, deleteRouteLua, []string{key}, caller, raw)
						continue
					}
					if !validRoute(route) || route.Caller.String() != caller || r.keys(route.SessionID)[4] != key {
						failures = errors.Join(failures, errors.New("sessionstore: corrupt route metadata"))
						continue
					}
					candidates[route.SessionID] = append(candidates[route.SessionID], route)
				}
			}
			// Exactly one authoritative get/prune per session, independent of field count.
			leases := make(map[string]*redis.Cmd)
			pipe = client.Pipeline()
			for id := range candidates {
				leases[id] = pipe.Eval(ctx, sessionLua, r.keys(id), "get", "", "", 1, r.retention.Milliseconds())
			}
			_, leaseErr := pipe.Exec(ctx)
			failures = errors.Join(failures, leaseErr)
			reachedLimit := false
			for id, cmd := range leases {
				result, err := cmd.Slice()
				if err != nil {
					continue
				}
				var lease Lease
				if len(result) > 0 && result[0] == int64(1) {
					lease, err = decodeLease(id, result)
				}
				if err != nil {
					failures = errors.Join(failures, err)
					continue
				}
				for _, route := range candidates[id] {
					if lease.Epoch != route.Generation || !time.Now().Before(route.ExpiresAt) {
						raw, _ := routeJSON(route)
						cleanup.Eval(ctx, deleteRouteLua, []string{r.keys(id)[4]}, route.Caller.String(), raw)
						continue
					}
					mu.Lock()
					if len(routes) >= limit {
						reachedLimit = true
					} else {
						routes = append(routes, route)
						owners[id] = lease
					}
					mu.Unlock()
				}
			}
			_, cleanupErr := cleanup.Exec(ctx)
			failures = errors.Join(failures, cleanupErr)
			if reachedLimit {
				return errors.Join(failures, ErrRouteLimit)
			}
			if failures != nil {
				return failures
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
	return routes, owners, err
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
func (r *RedisOwners) LoadRouteOwners(ctx context.Context, limit int) ([]Route, map[string]Lease, error) {
	return r.leases.LoadRouteOwners(ctx, limit)
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
