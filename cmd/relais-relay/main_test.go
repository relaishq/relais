package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestRelayCommandHelper(t *testing.T) {
	if os.Getenv("RELAIS_W43_COMMAND_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("relay", flag.ExitOnError)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}
func commandStore(t *testing.T) (*sessionstore.RedisOwners, string, string) {
	t.Helper()
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	require.NoError(t, processrun.ValidateRedis(addr))
	prefix := "relay-command-test:" + rand.Text() + ":"
	s, err := sessionstore.NewRedisOwners(context.Background(), storage.RedisConfig{Addr: addr, Prefix: prefix})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s, addr, prefix
}
func startCommand(t *testing.T, addr, prefix string, args ...string) *clusterprocess.Child {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	env := append(os.Environ(), "RELAIS_W43_COMMAND_HELPER=1")
	command := []string{binary, "-test.run=^TestRelayCommandHelper$", "--", "-redis", addr, "-redis-prefix", prefix}
	c, err := clusterprocess.Start(t.TempDir(), "relay", env, append(command, args...)...)
	require.NoError(t, err)
	t.Cleanup(c.Stop)
	return c
}
func readyCommand(t *testing.T, c *clusterprocess.Child) clusterprocess.Ready {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := c.Ready(ctx)
	require.NoError(t, err)
	return r
}
func freeUDP(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	a := c.LocalAddr().String()
	require.NoError(t, c.Close())
	return a
}
func TestCommandMismatchedIdentityNeverBinds(t *testing.T) {
	s, addr, prefix := commandStore(t)
	target, err := clusterprocess.Start(t.TempDir(), "target", os.Environ(), "sleep", "30")
	require.NoError(t, err)
	t.Cleanup(target.Stop)
	l, err := s.ClaimRelay(context.Background(), "default", sessionstore.RelayProcess{Owner: "target", PID: target.PID(), Start: "mismatched-start"}, 30*time.Millisecond)
	require.NoError(t, err)
	_, err = s.ActivateRelay(context.Background(), l)
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)
	public, leg := freeUDP(t), freeUDP(t)
	httpAddr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	c := startCommand(t, addr, prefix, "-standby", "-media", public, "-leg", leg, "-http", httpAddr)
	select {
	case <-c.Done():
		require.Error(t, c.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("mismatched fencing did not stop")
	}
	require.NoError(t, target.SignalGroup(0), "identity mismatch must not signal target")
	for _, a := range []string{public, leg} {
		socket, err := net.ListenPacket("udp", a)
		require.NoError(t, err, "no UDP bind after failed fence")
		require.NoError(t, socket.Close())
	}
	listener, err := net.Listen("tcp", httpAddr)
	require.NoError(t, err, "no HTTP bind after failed fence")
	require.NoError(t, listener.Close())
	current, err := s.GetRelay(context.Background(), "default")
	require.NoError(t, err)
	require.Equal(t, l.Holder, current.Forwarder)
}
func TestCommandSelfFencesOnObservedSuccessor(t *testing.T) {
	s, addr, prefix := commandStore(t)
	c := startCommand(t, addr, prefix)
	r := readyCommand(t, c)
	remote := &controlplane.RemoteRelay{URL: r.HTTP}
	status, err := remote.Status(context.Background())
	require.NoError(t, err)
	l, err := s.GetRelay(context.Background(), "default")
	require.NoError(t, err)
	require.Equal(t, status.Instance, l.Holder.Owner)
	_, err = s.TransferRelay(context.Background(), l, sessionstore.RelayProcess{Owner: "successor", PID: 42, Start: "test-start"}, time.Second)
	require.NoError(t, err)
	select {
	case <-c.Done():
		require.Error(t, c.Err())
	case <-time.After(time.Second):
		t.Fatal("active did not exit on observed lease loss")
	}
	for _, addr := range []string{r.Media, r.Leg} {
		socket, err := net.ListenPacket("udp", addr)
		require.NoError(t, err)
		require.NoError(t, socket.Close())
	}
}

// redisProxy cuts all existing Redis transports and refuses future ones. It
// isolates the outage test without stopping or changing the shared test Redis.
type redisProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	offline  bool
	closed   bool
	conns    []net.Conn
	wg       sync.WaitGroup
}

func newRedisProxy(t *testing.T, target string) *redisProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &redisProxy{listener: listener, target: target}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			in, err := listener.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go p.forward(in)
		}
	}()
	t.Cleanup(func() {
		p.mu.Lock()
		p.closed = true
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		_ = listener.Close()
		p.wg.Wait()
	})
	return p
}
func (p *redisProxy) forward(in net.Conn) {
	defer p.wg.Done()
	defer func() { _ = in.Close() }()
	out, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer func() { _ = out.Close() }()
	p.mu.Lock()
	if p.offline || p.closed {
		p.mu.Unlock()
		return
	}
	p.conns = append(p.conns, in, out)
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(out, in); _ = out.Close(); close(done) }()
	_, _ = io.Copy(in, out)
	_ = in.Close()
	<-done
}
func (p *redisProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offline = true
	for _, c := range p.conns {
		_ = c.Close()
	}
}
func TestRedisUnavailableNoStandbyTakeover(t *testing.T) {
	_, addr, prefix := commandStore(t)
	p := newRedisProxy(t, addr)
	proxyAddr := p.listener.Addr().String()
	active := startCommand(t, proxyAddr, prefix)
	r := readyCommand(t, active)
	standby := startCommand(t, proxyAddr, prefix, "-standby", "-media", r.Media, "-leg", r.Leg, "-http", strings.TrimPrefix(r.HTTP, "http://"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, standby.WaitLog(ctx, "STANDBY_WAIT"))
	remote := &controlplane.RemoteRelay{URL: r.HTTP}
	before, err := remote.Status(ctx)
	require.NoError(t, err)
	p.cut()
	time.Sleep(900 * time.Millisecond) // beyond the 600 ms lease lifetime
	after, err := remote.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Instance, after.Instance)
	require.Zero(t, after.Stats.SelfFences, "expiry alone cannot self-fence during Redis outage")
	select {
	case <-active.Done():
		t.Fatal("active exited during Redis outage")
	default:
	}
	select {
	case <-standby.Done():
		t.Fatal("waiting standby exited during Redis outage")
	default:
	}
	require.NotEqual(t, active.PID(), standby.PID())
	// All three original addresses still belong to the active relay, which
	// cannot be fenced because the standby has no confirmed lease decision.
	for _, addr := range []string{r.Media, r.Leg} {
		socket, err := net.ListenPacket("udp", addr)
		if socket != nil {
			_ = socket.Close()
		}
		require.Error(t, err)
	}
	listener, err := net.Listen("tcp", strings.TrimPrefix(r.HTTP, "http://"))
	if listener != nil {
		_ = listener.Close()
	}
	require.Error(t, err)
}

func TestValidateAddressesBeforeFencing(t *testing.T) {
	require.NoError(t, validateAddresses(true, "127.0.0.1:5000", "127.0.0.1:5001", "127.0.0.1:5002"))
	require.NoError(t, validateAddresses(false, "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"))
	for _, addrs := range [][3]string{
		{"127.0.0.1:0", "127.0.0.1:5001", "127.0.0.1:5002"},
		{"0.0.0.0:5000", "127.0.0.1:5001", "127.0.0.1:5002"},
		{"127.0.0.1:5000", "127.0.0.1:5001", "0.0.0.0:5002"},
		{"127.0.0.1:5000", "127.0.0.1:5001", "127.0.0.1:65536"},
	} {
		require.Error(t, validateAddresses(true, addrs[0], addrs[1], addrs[2]))
	}
}

func TestCommandWaitsForSocketRelease(t *testing.T) {
	for _, blocked := range []string{"public", "leg", "http"} {
		t.Run(blocked, func(t *testing.T) {
			_, addr, prefix := commandStore(t)
			public, leg := freeUDP(t), freeUDP(t)
			httpAddr, err := clusterprocess.FreeTCP()
			require.NoError(t, err)
			var held io.Closer
			if blocked == "http" {
				held, err = net.Listen("tcp", httpAddr)
			} else {
				udpAddr := public
				if blocked == "leg" {
					udpAddr = leg
				}
				held, err = net.ListenPacket("udp", udpAddr)
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = held.Close() })
			c := startCommand(t, addr, prefix, "-standby", "-media", public, "-leg", leg, "-http", httpAddr)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := c.WaitLog(ctx, "STANDBY_WAIT"); err != nil {
				_, details := c.Ready(ctx)
				require.NoError(t, err, "startup details: %v", details)
			}
			select {
			case <-c.Done():
				t.Fatalf("relay exited instead of waiting for %s release: %v", blocked, c.Err())
			case <-time.After(100 * time.Millisecond):
			}
			require.NoError(t, held.Close())
			r := readyCommand(t, c)
			status, err := (&controlplane.RemoteRelay{URL: r.HTTP}).Status(ctx)
			require.NoError(t, err)
			// Bind wait must be reported in the actual HTTP status response.
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.HTTP+"/status", nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			var wire struct {
				Continuity struct {
					BindWait time.Duration `json:"bind_wait_ns"`
				} `json:"continuity"`
			}
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&wire))
			require.Positive(t, wire.Continuity.BindWait)
			require.GreaterOrEqual(t, status.Continuity.BindRestore, wire.Continuity.BindWait)
		})
	}
}

func TestCommandBindBudgetFailsClosed(t *testing.T) {
	for _, blocked := range []string{"public", "leg", "http"} {
		t.Run(blocked, func(t *testing.T) {
			_, addr, prefix := commandStore(t)
			public, leg := freeUDP(t), freeUDP(t)
			httpAddr, err := clusterprocess.FreeTCP()
			require.NoError(t, err)
			var held io.Closer
			if blocked == "http" {
				held, err = net.Listen("tcp", httpAddr)
			} else {
				udpAddr := public
				if blocked == "leg" {
					udpAddr = leg
				}
				held, err = net.ListenPacket("udp", udpAddr)
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = held.Close() })
			c := startCommand(t, addr, prefix, "-standby", "-media", public, "-leg", leg, "-http", httpAddr, "-relay-bind-timeout", "80ms")
			select {
			case <-c.Done():
				require.Error(t, c.Err())
			case <-time.After(3 * time.Second):
				t.Fatal("bind budget did not stop the candidate")
			}
			_, details := c.Ready(context.Background())
			require.ErrorContains(t, details, "bind_failures=1")
			require.ErrorContains(t, details, "context deadline exceeded")
			require.NotContains(t, details.Error(), `"instance":`, "failed candidate must never become ready")
			for name, a := range map[string]string{"public": public, "leg": leg} {
				if name == blocked {
					continue
				}
				socket, err := net.ListenPacket("udp", a)
				require.NoError(t, err, "partial UDP bind must be released")
				require.NoError(t, socket.Close())
			}
			if blocked != "http" {
				listener, err := net.Listen("tcp", httpAddr)
				require.NoError(t, err, "partial HTTP bind must be released")
				require.NoError(t, listener.Close())
			}
		})
	}
}

func TestCommandLeaseLossCancelsBindWait(t *testing.T) {
	s, addr, prefix := commandStore(t)
	held, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = held.Close() }()
	httpAddr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	c := startCommand(t, addr, prefix, "-standby", "-media", held.LocalAddr().String(), "-leg", freeUDP(t), "-http", httpAddr, "-relay-bind-timeout", "5s")
	var l sessionstore.RelayLease
	require.Eventually(t, func() bool {
		l, err = s.GetRelay(context.Background(), "default")
		return err == nil && l.Forwarder == l.Holder
	}, time.Second, 10*time.Millisecond)
	_, err = s.TransferRelay(context.Background(), l, sessionstore.RelayProcess{Owner: "successor", PID: 42, Start: "test-start"}, time.Second)
	require.NoError(t, err)
	select {
	case <-c.Done():
		require.Error(t, c.Err())
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel the five-second bind wait")
	}
	listener, err := net.Listen("tcp", httpAddr)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}
