package sessionstore

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

type declaredScriptCall struct {
	script string
	keys   []string
	op     string
}
type keyDeclarationHook struct {
	mu    sync.Mutex
	calls []declaredScriptCall
}

func (h *keyDeclarationHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	if (cmd.Name() == "eval" || cmd.Name() == "evalsha") && len(args) >= 3 {
		n, _ := strconv.Atoi(fmt.Sprint(args[2]))
		call := declaredScriptCall{script: fmt.Sprint(args[1])}
		for _, k := range args[3 : 3+n] {
			call.keys = append(call.keys, fmt.Sprint(k))
		}
		argv := sessionCommandArgs(cmd)
		if len(argv) > 0 {
			call.op = fmt.Sprint(argv[0])
		}
		h.mu.Lock()
		h.calls = append(h.calls, call)
		h.mu.Unlock()
	}
	return ctx, nil
}
func (*keyDeclarationHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*keyDeclarationHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*keyDeclarationHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }
func TestRedisScriptSlotDeclarations(t *testing.T) {
	forComponentRedis(t, func(t *testing.T, r *Redis) {
		ctx := context.Background()
		h := &keyDeclarationHook{}
		r.client.AddHook(h)
		lease, err := r.Claim(ctx, "declared", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		require.NoError(t, r.PutState(ctx, lease, []byte("legacy")))
		_, err = r.Clock(ctx, lease.SessionID)
		require.NoError(t, err)
		lease, err = r.Claim(ctx, "component-declared", lease.Worker, time.Minute)
		require.NoError(t, err)
		tuple, err := r.CommitComponents(ctx, lease, ComponentCommit{Components: componentFixture(lease.SessionID), StateVersion: 1})
		require.NoError(t, err)
		for range 2 {
			c := cloneComponents(tuple.Components)
			c.Work = appendComponentBytes(t, c.Work, 3, 1)
			c.History = appendComponentBytes(t, c.History, 4, 1)
			tuple, err = r.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1, Expected: &tuple.Metadata})
			require.NoError(t, err)
			_, err = r.ReadComponents(ctx, lease.SessionID)
			require.NoError(t, err)
		}
		r.maintenance.Wait()
		h.mu.Lock()
		calls := append([]declaredScriptCall(nil), h.calls...)
		h.mu.Unlock()
		seen := map[string]bool{}
		for _, call := range calls {
			switch call.script {
			case componentCommitScript.Hash(), componentReadScript.Hash(), componentCommitLua, componentReadLua:
				require.Equal(t, r.componentKeys(lease.SessionID), call.keys)
				seen["component"] = true
				// Every computed index is exercised with both mutable app banks. The
				// declaration must include the entire family even for an unchanged part.
				for bank := 0; bank < 2; bank++ {
					for i := 1; i <= 2; i++ {
						index := 6 + i*2 + bank
						require.LessOrEqual(t, index, len(call.keys))
						require.Equal(t, r.componentKeys(lease.SessionID)[index-1], call.keys[index-1])
					}
				}
				for bank := 0; bank < 2; bank++ {
					require.Contains(t, call.keys, r.componentKeys(lease.SessionID)[8+bank-1])
					require.Contains(t, call.keys, r.componentKeys(lease.SessionID)[10+bank-1])
				}
			case sessionScript.Hash(), sessionLua:
				if call.op == "sequence" {
					require.Len(t, call.keys, 1)
					seen["sequence"] = true
				}
				if call.op == "put" {
					require.Equal(t, r.keys("declared")[:2], call.keys)
					seen["put"] = true
				}
			case storeClockLua:
				require.Equal(t, r.keys("declared")[:1], call.keys)
				seen["clock"] = true
			}
			redistestcluster.SameSlot(t, call.keys...)
		}
		require.Equal(t, map[string]bool{"component": true, "sequence": true, "put": true, "clock": true}, seen)
	})
}
func TestRedisComputedSlotIndexesAreExplicitlyAudited(t *testing.T) {
	allowed := map[string]bool{"i": true, "6+i*2+bank(m,i)": true, "8+w": true, "10+h": true}
	for _, lua := range []string{sessionLua, componentCommitLua, componentReadLua} {
		for _, m := range regexp.MustCompile(`KEYS\[([^\]]+)\]`).FindAllStringSubmatch(lua, -1) {
			if _, err := strconv.Atoi(m[1]); err != nil {
				require.True(t, allowed[m[1]], "new computed key index needs a production-call slot audit: %s", m[1])
			}
		}
	}
}
