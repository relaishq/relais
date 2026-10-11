package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/callharness"
)

type topology struct {
	External  callharness.ExternalTopology
	Workers   map[int]*clusterprocess.Child
	processes []*clusterprocess.Child
}

// Owned loopback topology; caller attachment is separate so namespace and
// externally owned topologies can supply the same caller package later.
func startTopology(ctx context.Context, m *clusterprocess.Manager, bin, dir string, workers int) (_ *topology, runErr error) {
	t := &topology{Workers: map[int]*clusterprocess.Child{}}
	defer func() {
		if runErr != nil {
			t.close()
		}
	}()
	addr, err := clusterprocess.FreeTCP()
	if err != nil {
		return nil, err
	}
	if err := processrun.ValidateRedis(addr); err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr)
	redis, err := m.Start(dir, "redis", os.Environ(), "redis-server", "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
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
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	env := serviceEnv(hex.EncodeToString(key[:]), addr, "relais:load:"+filepath.Base(dir)+":")
	start := func(name string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		childEnv := env
		if name == "relay" {
			childEnv = serviceEnv("", addr, "relais:load:"+filepath.Base(dir)+":")
		}
		for i := 1; i < len(args); i++ {
			if args[i] != "127.0.0.1:0" {
				continue
			}
			var err error
			args[i], err = clusterprocess.FreeTCP()
			if err != nil {
				return nil, clusterprocess.Ready{}, err
			}
			if args[i-1] == "-media" || args[i-1] == "-leg" {
				socket, err := net.ListenPacket("udp4", args[i])
				if err != nil {
					return nil, clusterprocess.Ready{}, err
				}
				_ = socket.Close()
			}
		}
		c, err := m.Start(dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		t.processes = append(t.processes, c)
		startup, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		ready, err := c.Ready(startup)
		return c, ready, err
	}
	_, relay, err := start("relay", filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	controlAddr, err := clusterprocess.FreeTCP()
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
		c, ready, err := start("worker-"+name, filepath.Join(bin, "relais-worker"), "-media", "127.0.0.1:0", "-http", "127.0.0.1:0", "-name", name, "-control", controlURL, "-relay-leg", relay.Leg, "-relay-media", relay.Media)
		if err != nil {
			return nil, err
		}
		t.Workers[i] = c
		args = append(args, "-worker", name+"="+ready.HTTP)
	}
	_, _, err = start("control", args...)
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
	return c.SignalGroup(syscall.SIGKILL)
}

func (t *topology) close() {
	for i := len(t.processes) - 1; i >= 0; i-- {
		t.processes[i].Stop()
	}
}
