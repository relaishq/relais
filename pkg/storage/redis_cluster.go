package storage

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/go-redis/redis/v8"
)

// RedisClusterOptions preserves the caller's transport and retry policy while
// accepting the extensible CLUSTER SLOTS node tuples used by Redis 7+ and Valkey.
// go-redis v8's typed decoder only accepts Redis 6's two/three-element tuples.
// Discovery follows remembered nodes as well as seeds, including after failover.
// Authority reads always use primaries; replica/latency/random routing is off.
func RedisClusterOptions(options *redis.UniversalOptions) *redis.ClusterOptions {
	cluster := options.Cluster()
	cluster.NewClient = func(opts *redis.Options) *redis.Client { return RedisClusterNode(redis.NewClient(opts)) }
	cluster.ReadOnly, cluster.RouteByLatency, cluster.RouteRandomly = false, false, false
	var mu sync.Mutex
	known := append([]string(nil), options.Addrs...)
	cluster.ClusterSlots = func(ctx context.Context) ([]redis.ClusterSlot, error) {
		mu.Lock()
		addrs := append([]string(nil), known...)
		mu.Unlock()
		var last error
		for _, addr := range addrs {
			opts := options.Simple()
			opts.Addr, opts.MaxRetries = addr, -1
			client := redis.NewClient(opts)
			reply, err := client.Do(ctx, "CLUSTER", "SLOTS").Slice()
			_ = client.Close()
			if err != nil {
				last = err
				if ctx.Err() != nil {
					break
				}
				continue
			}
			slots, err := decodeClusterSlots(reply, addr)
			if err != nil {
				last = err
				continue
			}
			seen := make(map[string]bool)
			next := make([]string, 0, len(addrs))
			for _, slot := range slots {
				for _, node := range slot.Nodes {
					if !seen[node.Addr] {
						seen[node.Addr] = true
						next = append(next, node.Addr)
					}
				}
			}
			for _, seed := range options.Addrs {
				if !seen[seed] {
					next = append(next, seed)
				}
			}
			mu.Lock()
			known = next
			mu.Unlock()
			return slots, nil
		}
		if last == nil {
			last = errors.New("no Redis Cluster seeds")
		}
		return nil, last
	}
	return cluster
}

func decodeClusterSlots(reply []interface{}, source string) ([]redis.ClusterSlot, error) {
	slots := make([]redis.ClusterSlot, 0, len(reply))
	for _, raw := range reply {
		row, ok := raw.([]interface{})
		if !ok || len(row) < 3 {
			return nil, errors.New("invalid Redis Cluster slot tuple")
		}
		start, sok := row[0].(int64)
		end, eok := row[1].(int64)
		if !sok || !eok || start < 0 || end < start || end >= 16384 {
			return nil, errors.New("invalid Redis Cluster slot range")
		}
		slot := redis.ClusterSlot{Start: int(start), End: int(end)}
		for _, rawNode := range row[2:] {
			node, ok := rawNode.([]interface{})
			if !ok || len(node) < 2 {
				return nil, errors.New("invalid Redis Cluster node tuple")
			}
			host, hostOK := node[0].(string)
			if node[0] == nil || hostOK && host == "" {
				host, _, _ = net.SplitHostPort(source)
				hostOK = true
			}
			port, portOK := node[1].(int64)
			if !hostOK || !portOK || port < 1 || port > 65535 {
				return nil, errors.New("invalid Redis Cluster node endpoint")
			}
			id := ""
			if len(node) > 2 {
				id, ok = node[2].(string)
				if !ok {
					return nil, errors.New("invalid Redis Cluster node ID")
				}
			}
			// Additional metadata is deliberately ignored, not interpreted as authority.
			slot.Nodes = append(slot.Nodes, redis.ClusterNode{ID: id, Addr: net.JoinHostPort(host, strconv.FormatInt(port, 10))})
		}
		slots = append(slots, slot)
	}
	if len(slots) == 0 {
		return nil, errors.New("redis cluster returned no slots")
	}
	return slots, nil
}

// RedisClusterNode equips a node with the Redis 7+ COMMAND metadata decoder.
// Only discovery replies are adapted; application commands retain their errors
// and the caller's retry policy. Redis 6's existing typed decoder is unchanged.
func RedisClusterNode(client *redis.Client) *redis.Client {
	client.AddHook(clusterCommandMetadata{client: client})
	return client
}

type clusterCommandMetadata struct{ client *redis.Client }

func (h clusterCommandMetadata) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h clusterCommandMetadata) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h clusterCommandMetadata) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}
func (h clusterCommandMetadata) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	metadata, ok := cmd.(*redis.CommandsInfoCmd)
	if !ok || cmd.Err() == nil || !strings.Contains(cmd.Err().Error(), "elements in COMMAND reply") {
		return nil
	}
	// A generic RESP command accepts the additional Redis 7 fields. The typed
	// command's failed read discards its connection, so no unread data is reused.
	reply, err := h.client.Do(ctx, "COMMAND").Slice()
	if err != nil {
		return err
	}
	info, err := decodeClusterCommands(reply)
	if err != nil {
		return err
	}
	metadata.SetVal(info)
	metadata.SetErr(nil)
	return nil
}
func decodeClusterCommands(reply []interface{}) (map[string]*redis.CommandInfo, error) {
	commands := make(map[string]*redis.CommandInfo, len(reply))
	for _, raw := range reply {
		row, ok := raw.([]interface{})
		if !ok || len(row) < 6 {
			return nil, errors.New("invalid Redis command metadata")
		}
		name, nok := row[0].(string)
		arity, aok := row[1].(int64)
		flags, fok := row[2].([]interface{})
		first, fiok := row[3].(int64)
		last, lok := row[4].(int64)
		step, sok := row[5].(int64)
		if !nok || !aok || !fok || !fiok || !lok || !sok {
			return nil, errors.New("invalid Redis command fields")
		}
		command := &redis.CommandInfo{Name: name, Arity: int8(arity), FirstKeyPos: int8(first), LastKeyPos: int8(last), StepCount: int8(step)}
		for _, rawFlag := range flags {
			flag, ok := rawFlag.(string)
			if !ok {
				return nil, errors.New("invalid Redis command flag")
			}
			command.Flags = append(command.Flags, flag)
			if flag == "readonly" {
				command.ReadOnly = true
			}
		}
		commands[name] = command
	}
	return commands, nil
}
