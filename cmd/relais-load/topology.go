package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/workerdriver"
	"github.com/relaishq/relais/pkg/callharness"
)

type workerLaunch struct {
	Env, Args   []string
	URL         string
	Incarnation int
}

type topology struct {
	Trace                                 storeTrace
	Observer                              *storeObserver
	OldKey, NewKey                        [32]byte
	Sentinel                              int
	Rejoins                               []loadgen.WorkerRejoin
	Launch                                map[int]*workerLaunch
	Manager                               *clusterprocess.Manager
	Dir, RedisAddr, RedisPassword, Prefix string
	Soak                                  *soakExecution
	External                              callharness.ExternalTopology
	Workers                               map[int]*clusterprocess.Child
	Killed                                map[int]bool
	statusRequests                        uint64
	processes                             []*clusterprocess.Child
	Network                               *nettopology.Topology
	Targets                               []capacity.Target
	Monitor                               *capacity.Monitor
}

// Owned loopback or Linux namespace topology. Caller attachment is separate
// so only the media socket enters the caller namespace.
func startTopology(ctx context.Context, m *clusterprocess.Manager, bin, dir string, workers int, mode string, selections ...workerdriver.Options) (*topology, error) {
	var worker workerdriver.Options
	if len(selections) > 0 {
		worker = selections[0]
	}
	return startConfiguredTopology(ctx, m, bin, dir, workers, mode, worker, false)
}
func startConfiguredTopology(ctx context.Context, m *clusterprocess.Manager, bin, dir string, workers int, mode string, worker workerdriver.Options, observe bool) (_ *topology, runErr error) {
	t := &topology{Workers: map[int]*clusterprocess.Child{}, Launch: map[int]*workerLaunch{}, Manager: m, Dir: dir}
	defer func() {
		if runErr != nil {
			runErr = errors.Join(runErr, t.close())
		}
	}()
	var err error
	if mode == "netns" {
		t.Network, err = nettopology.OpenManagedWorkers(ctx, func(format string, args ...any) { fmt.Printf(format+"\n", args...) }, m.AddCleanup, workers)
		if err != nil {
			return nil, err
		}
	}
	addr, err := redisAddress(t.Network)
	if err != nil {
		return nil, err
	}
	if err := redisendpoint.ValidateDev(addr); err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr)
	password := ""
	if t.Network != nil {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, err
		}
		password = hex.EncodeToString(secret[:])
	}
	redisArgs, removeConfig, err := redisCommand(t.Network, "redis-server", port, dir, password)
	if err != nil {
		return nil, err
	}
	defer removeConfig()
	redis, err := startRole(m, t.Network, dir, "redis", os.Environ(), redisArgs...)
	if err != nil {
		return nil, err
	}
	t.processes = append(t.processes, redis)
	startup, stop := context.WithTimeout(ctx, 10*time.Second)
	err = clusterprocess.WaitTCP(startup, addr, redis)
	stop()
	if err != nil {
		return nil, err
	}
	if err := pingRedis(ctx, addr, password); err != nil {
		return nil, err
	}
	t.Targets = append(t.Targets, capacity.Target{Name: "store", PID: redis.PID(), Done: redis.Done(), Err: redis.Err, RedisAddr: addr, RedisPassword: password})
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	t.RedisAddr, t.RedisPassword, t.Prefix = addr, password, "relais:load:"+filepath.Base(dir)+":"
	if observe {
		t.Prefix = "relais:load:matched:"
	}
	t.OldKey = key
	serviceAddr := addr
	if observe {
		t.Sentinel = -1
		if _, err := rand.Read(t.NewKey[:]); err != nil {
			return nil, err
		}
		t.Observer, err = startStoreObserver(addr)
		if err != nil {
			return nil, err
		}
		serviceAddr = t.Observer.listener.Addr().String()
	}
	env := serviceEnv(hex.EncodeToString(key[:]), serviceAddr, t.Prefix)
	if observe {
		env = append(env, "RELAIS_SESSIONSTORE_KEY_ID=1", `RELAIS_SESSIONSTORE_OLD_KEYS={"2":"`+hex.EncodeToString(t.NewKey[:])+`"}`)
	}
	env = append(env, "RELAIS_REDIS_PASSWORD="+password)
	if t.Network != nil {
		env = append(env, "RELAIS_PRIVATE_NETS="+t.Network.Plan.PrivateNets())
	}
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := env
		if name == "relay" {
			childEnv = withoutSessionKeys(env)
		}
		for i := 1; i < len(args); i++ {
			if !strings.HasSuffix(args[i], ":0") {
				continue
			}
			var err error
			if t.Network == nil {
				args[i], err = clusterprocess.FreeTCP()
			} else {
				host, _, _ := net.SplitHostPort(args[i])
				args[i] = net.JoinHostPort(host, fmt.Sprint(40000+i))
			}
			if err != nil {
				return nil, clusterprocess.Ready{}, err
			}
			if t.Network == nil && (args[i-1] == "-media" || args[i-1] == "-leg") {
				socket, err := net.ListenPacket("udp4", args[i])
				if err != nil {
					return nil, clusterprocess.Ready{}, err
				}
				_ = socket.Close()
			}
		}
		c, err := startRole(m, t.Network, dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		t.processes = append(t.processes, c)
		startup, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		ready, err := c.Ready(startup)
		if err == nil {
			if strings.HasPrefix(name, "worker-") {
				id, _ := strconv.Atoi(strings.TrimPrefix(name, "worker-"))
				t.Launch[id] = &workerLaunch{Env: childEnv, Args: append([]string(nil), args...), URL: ready.HTTP}
			}
			t.Targets = append(t.Targets, capacity.Target{Name: name, PID: c.PID(), URL: ready.HTTP, Done: c.Done(), Err: c.Err})
		}
		return c, ready, err
	}
	_, relay, err := start("relay", filepath.Join(bin, "relais-relay"), "-media", roleBind(t.Network, "relay", true), "-leg", roleBind(t.Network, "relay", false), "-http", roleBind(t.Network, "relay", false))
	if err != nil {
		return nil, err
	}
	controlAddr, err := controlAddress(t.Network)
	if err != nil {
		return nil, err
	}
	controlURL := "http://" + controlAddr
	relayAddr, err := netip.ParseAddrPort(relay.Media)
	if err != nil {
		return nil, err
	}
	t.External = callharness.ExternalTopology{SignalingURL: controlURL + "/calls", RelayAddr: relayAddr}
	args := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", relay.HTTP}
	for i := range workers {
		name := fmt.Sprint(i)
		c, ready, err := start("worker-"+name, worker.Command(bin, "-media", roleBind(t.Network, "worker-"+name, false), "-http", roleBind(t.Network, "worker-"+name, false), "-name", name, "-control", controlURL, "-relay-leg", relay.Leg, "-relay-media", relay.Media)...)
		if err != nil {
			return nil, err
		}
		t.Workers[i] = c
		args = append(args, "-worker", name+"="+ready.HTTP)
	}
	_, _, err = start("control", args...)
	if err == nil && t.Network != nil {
		err = t.Network.AllowMedia(ctx, relayAddr)
		var addresses []string
		for _, target := range t.Targets {
			if target.URL != "" {
				addresses = append(addresses, strings.TrimPrefix(target.URL, "http://"))
			}
		}
		addresses = append(addresses, addr)
		if err == nil {
			err = t.Network.VerifyPrivateAPIs(ctx, addresses)
		}
	}
	return t, err
}
func serviceEnv(key, addr, prefix string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "RELAIS_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "RELAIS_REDIS_ADDR="+addr, "RELAIS_REDIS_PREFIX="+prefix)
	if key != "" {
		env = append(env, "RELAIS_SESSIONSTORE_KEY="+key)
	}
	return env
}
func (t *topology) kill(worker int) error {
	c := t.Workers[worker]
	if c == nil {
		return fmt.Errorf("worker %d is not owned", worker)
	}
	if t.Soak != nil {
		t.Soak.retire(fmt.Sprintf("worker-%d", worker))
	}
	if t.Monitor != nil {
		t.Monitor.Retire(fmt.Sprintf("worker-%d", worker))
	}
	if err := c.SignalGroup(syscall.SIGKILL); err != nil {
		return err
	}
	if t.Killed == nil {
		t.Killed = map[int]bool{}
	}
	t.Killed[worker] = true
	return nil
}

func (t *topology) close() error {
	for i := len(t.processes) - 1; i >= 1; i-- {
		t.processes[i].Stop()
	}
	if t.Observer != nil {
		t.Trace = t.Observer.close()
		t.Observer = nil
	}
	if len(t.processes) > 0 {
		t.processes[0].Stop()
	}
	if t.Network != nil {
		return t.Network.Close()
	}
	return nil
}

func withoutSessionKeys(env []string) []string {
	var out []string
	for _, entry := range env {
		if !strings.HasPrefix(entry, "RELAIS_SESSIONSTORE_") {
			out = append(out, entry)
		}
	}
	return out
}
