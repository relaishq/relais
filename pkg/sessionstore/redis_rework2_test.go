package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-redis/redis/v8"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConcurrentListingAndTransitions(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		s := factory(t)
		ctx := context.Background()
		a := netip.MustParseAddrPort("127.0.0.1:1")
		b := netip.MustParseAddrPort("127.0.0.1:2")
		done := make(chan struct{})
		var listings sync.WaitGroup
		listings.Add(1)
		go func() {
			defer listings.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, w := range []netip.AddrPort{a, b} {
					_, err := s.ListByWorker(ctx, w)
					if err != nil {
						t.Errorf("listing: %v", err)
						return
					}
				}
			}
		}()
		var work sync.WaitGroup
		var failures atomic.Int32
		for g := range 8 {
			work.Go(func() {
				for i := range 200 {
					id := fmt.Sprintf("concurrent-%d-%d", g, i)
					lease, err := s.Claim(ctx, id, a, time.Minute)
					if err == nil {
						lease, err = s.Transfer(ctx, lease, b, time.Minute)
					}
					if err == nil {
						err = s.Release(ctx, lease)
					}
					if err != nil {
						failures.Add(1)
					}
				}
			})
		}
		work.Wait()
		close(done)
		listings.Wait()
		t.Logf("1600 claim/transfer/release cycles: spurious failures=%d", failures.Load())
		require.Zero(t, failures.Load())
	})
}

// Exercise the public retries using definitive server-side epoch fences.
type overtakeHook struct {
	lostResponseHook
	store    *Redis
	op       string
	attempts int
	epochs   []string
}

func (h *overtakeHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := sessionCommandArgs(cmd)
	if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) > 2 && args[0] == h.op {
		epoch := fmt.Sprint(args[2])
		worker := fmt.Sprint(args[1])
		id := "retry-claim"
		if h.op == "transfer" {
			worker = fmt.Sprint(args[5])
			epoch = fmt.Sprint(args[6])
			id = "retry-transfer"
		}
		h.attempts++
		h.epochs = append(h.epochs, epoch)
		if h.attempts <= 2 {
			_, _, err := h.store.Settle(ctx, candidateLease(id, epoch, netip.MustParseAddrPort(worker)))
			return ctx, err
		}
	}
	return ctx, nil
}
func TestRedisRetriesOvertakenEpochWithFreshCandidates(t *testing.T) {
	for _, op := range []string{"claim", "transfer"} {
		t.Run(op, func(t *testing.T) {
			r := testRedis(t)
			ctx := context.Background()
			a := netip.MustParseAddrPort("127.0.0.1:1")
			b := netip.MustParseAddrPort("127.0.0.1:2")
			require.NoError(t, r.client.ScriptLoad(ctx, sessionLua).Err())
			var lease Lease
			var err error
			if op == "transfer" {
				lease, err = r.Claim(ctx, "retry-transfer", a, time.Minute)
				require.NoError(t, err)
			}
			r.maintenance.Wait()
			h := &overtakeHook{store: r, op: op}
			r.client.AddHook(h)
			if op == "claim" {
				lease, err = r.Claim(ctx, "retry-claim", a, time.Minute)
			} else {
				lease, err = r.Transfer(ctx, lease, b, time.Minute)
			}
			require.NoError(t, err)
			require.Equal(t, 3, h.attempts)
			require.Len(t, h.epochs, 3)
			require.NotEqual(t, h.epochs[0], h.epochs[1])
			require.NotEqual(t, h.epochs[1], h.epochs[2])
			listed, err := r.ListByWorker(ctx, lease.Worker)
			require.NoError(t, err)
			require.Equal(t, []Lease{lease}, listed)
		})
	}
}

// Count exact RESP request bytes for the same 1000-member pipeline using EVAL
// versus EVALSHA. This is a local request-size comparison, not a timing claim.
func respBytes(args []interface{}) int {
	n := len(fmt.Sprintf("*%d\r\n", len(args)))
	for _, arg := range args {
		s := fmt.Sprint(arg)
		n += len(fmt.Sprintf("$%d\r\n", len(s))) + len(s) + 2
	}
	return n
}

type listingWireHook struct {
	lostResponseHook
	bytes     int
	evalBytes int
	batches   int
	loads     int
	cold      bool
}

func (h *listingWireHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if cmd.Name() == "script" && cmd.Args()[1] == "load" {
		h.loads++
	}
	return ctx, nil
}
func (h *listingWireHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	h.batches++
	for _, cmd := range cmds {
		if cmd.Name() != "evalsha" {
			return ctx, errors.New("listing must use EVALSHA")
		}
		h.bytes += respBytes(cmd.Args())
		old := append([]interface{}(nil), cmd.Args()...)
		old[0] = "eval"
		old[1] = sessionLua
		h.evalBytes += respBytes(old)
	}
	if h.cold {
		h.cold = false
		return ctx, errors.New("NOSCRIPT simulated cold node")
	}
	return ctx, nil
}
func TestRedisListingWireSizeAndColdScriptRecovery(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	for i := range 1000 {
		_, err := r.Claim(ctx, fmt.Sprintf("listing-%04d", i), a, time.Minute)
		require.NoError(t, err)
	}
	r.maintenance.Wait()
	h := &listingWireHook{}
	r.client.AddHook(h)
	leases, err := r.ListByWorker(ctx, a)
	require.NoError(t, err)
	require.Len(t, leases, 1000)
	require.Equal(t, 1, h.batches)
	t.Logf("1000-member request bytes: EVAL=%d EVALSHA=%d saved=%.2f%%", h.evalBytes, h.bytes, 100*(1-float64(h.bytes)/float64(h.evalBytes)))
	h.cold = true
	h.loads = 0
	h.batches = 0
	leases, err = r.ListByWorker(ctx, a)
	require.NoError(t, err)
	require.Len(t, leases, 1000)
	require.Equal(t, 1, h.loads)
	require.Equal(t, 2, h.batches)
}

type slowRepairHook struct {
	lostResponseHook
	adds    atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (h *slowRepairHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if cmd.Name() == "eval" && cmd.Args()[1] == indexLua && h.adds.Add(1) == 2 {
		close(h.entered)
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx, ctx.Err()
		}
	}
	return ctx, nil
}
func TestRedisPostCommitRepairDoesNotDelayCaller(t *testing.T) {
	r := testRedis(t)
	h := &slowRepairHook{entered: make(chan struct{}), release: make(chan struct{})}
	r.client.AddHook(h)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(h.release) }) })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	started := time.Now()
	lease, err := r.Claim(ctx, "async-repair", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	require.Less(t, time.Since(started), 1500*time.Millisecond)
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("repair did not start")
	}
	current, err := r.Get(context.Background(), lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, lease, current)
	once.Do(func() { close(h.release) })
}
