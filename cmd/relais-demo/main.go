// relais-demo serves a browser demo with an owned or externally managed cluster.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/processrun"
)

//go:embed web
var web embed.FS

type config struct {
	Bin, HTTP, Redis, RedisBinary, LocalNetwork, Agent, RelayBuffer string
	AttachControl, AttachAuthFile, FaultHook, ResultsDir            string
	ActionTimeout                                                   time.Duration
}

func processEnv(key, addr, prefix string, withKey bool) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "RELAIS_SESSIONSTORE_KEY" || name == "RELAIS_REDIS_ADDR" || name == "RELAIS_REDIS_PREFIX" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "RELAIS_REDIS_ADDR="+addr, "RELAIS_REDIS_PREFIX="+prefix)
	if withKey {
		env = append(env, "RELAIS_SESSIONSTORE_KEY="+key)
	}
	return env
}

func launch(ctx context.Context, manager *clusterprocess.Manager, cfg config) (*demo, error) {
	if cfg.AttachControl != "" {
		return launchAttached(ctx, cfg)
	}
	id, err := newLaunchID()
	if err != nil {
		return nil, err
	}
	token, err := configToken(cfg)
	if err != nil {
		return nil, err
	}
	bin, err := filepath.Abs(cfg.Bin)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"relais-relay", "relais-worker", "relais-control"} {
		info, err := os.Stat(filepath.Join(bin, name))
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return nil, fmt.Errorf("build %s first (make build)", name)
		}
	}
	dir, err := os.MkdirTemp(bin, "demo-cluster-")
	if err != nil {
		return nil, err
	}
	fmt.Printf("process logs: %s\n", dir)
	addr := cfg.Redis
	if addr == "" {
		addr, err = clusterprocess.FreeTCP()
		if err != nil {
			return nil, err
		}
		if err := processrun.ValidateRedis(addr); err != nil {
			return nil, err
		}
		_, port, _ := net.SplitHostPort(addr)
		redis, err := manager.Start(dir, "redis", os.Environ(), cfg.RedisBinary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
		if err != nil {
			return nil, err
		}
		startup, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = redis.WaitLog(startup, "Ready to accept connections")
		cancel()
		if err != nil {
			return nil, err
		}
	}
	if err := processrun.ValidateRedis(addr); err != nil {
		return nil, err
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	prefix := "relais:demo:" + filepath.Base(dir) + ":"
	env := processEnv(hex.EncodeToString(key[:]), addr, prefix, true)
	start := func(startCtx context.Context, name string, childEnv []string, args ...string) (*clusterprocess.Child, clusterprocess.Ready, error) {
		child, err := manager.Start(dir, name, childEnv, args...)
		if err != nil {
			return nil, clusterprocess.Ready{}, err
		}
		startup, cancel := context.WithTimeout(startCtx, 10*time.Second)
		defer cancel()
		ready, err := readyChild(startup, child)
		return child, ready, err
	}
	_, relay, err := start(ctx, "relay", processEnv("", addr, prefix, false), append([]string{filepath.Join(bin, "relais-relay")}, relayArgs(cfg)...)...)
	if err != nil {
		return nil, err
	}
	controlAddr, err := clusterprocess.FreeTCP()
	if err != nil {
		return nil, err
	}
	d := &demo{ctx: ctx, client: &http.Client{Timeout: 10 * time.Second}, control: "http://" + controlAddr, relay: relay.Media, redis: addr, dir: dir, workers: map[string]*workerProcess{}, publicHost: cfg.LocalNetwork, launchToken: token, launchID: id, agentName: selectedAgent(cfg), relayBuffer: selectedRelayBuffer(cfg)}
	if err := d.prepareResults(cfg.ResultsDir); err != nil {
		return nil, err
	}
	d.actionTimeout = cfg.ActionTimeout
	usedMedia := map[string]bool{}
	d.spawn = func(ctx context.Context, name string) (*workerProcess, error) {
		media, err := freshUDP(usedMedia)
		if err != nil {
			return nil, err
		}
		workerEnv := make([]string, 0, len(env)+1)
		for _, entry := range env {
			if !strings.HasPrefix(entry, "PION_LOG_INFO=") {
				workerEnv = append(workerEnv, entry)
			}
		}
		workerEnv = append(workerEnv, "PION_LOG_INFO=session")
		child, ready, err := start(ctx, "worker-"+name, workerEnv, filepath.Join(bin, "relais-worker"), "-media", media, "-http", "127.0.0.1:0", "-name", name, "-control", d.control, "-relay-leg", relay.Leg, "-relay-media", relay.Media, "-agent", selectedAgent(cfg))
		if err != nil {
			return nil, err
		}
		return &workerProcess{child: child, URL: ready.HTTP}, nil
	}
	args := []string{filepath.Join(bin, "relais-control"), "-http", controlAddr, "-relay", relay.HTTP, "-demo-register"}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("%d", i)
		worker, err := d.spawn(ctx, name)
		if err != nil {
			return nil, err
		}
		d.workers[name] = worker
		args = append(args, "-worker", name+"="+worker.URL)
	}
	d.next = 3
	_, _, err = start(ctx, "control", env, args...)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// The control registry retains historical addresses. Avoid handing a fresh
// process a retired UDP address, even if the OS happens to allocate it again.
func freshUDP(used map[string]bool) (string, error) {
	for i := 0; i < 100; i++ {
		addr, err := clusterprocess.FreeUDP()
		if err != nil {
			return "", err
		}
		if !used[addr] {
			used[addr] = true
			return addr, nil
		}
	}
	return "", errors.New("could not allocate a new worker UDP address")
}

func selectedAgent(cfg config) string {
	if cfg.Agent == "" {
		return "echo"
	}
	return cfg.Agent
}

func newLaunchID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func selectedRelayBuffer(cfg config) string {
	if cfg.RelayBuffer == "" {
		return "on"
	}
	return cfg.RelayBuffer
}

func validateConfig(cfg config) error {
	if err := validateAttachConfig(cfg); err != nil {
		return err
	}
	if selectedRelayBuffer(cfg) != "on" && selectedRelayBuffer(cfg) != "off" {
		return errors.New("-relay-buffer must be on or off")
	}
	if selectedAgent(cfg) != "echo" && selectedAgent(cfg) != "demo" {
		return errors.New("-agent must be echo or demo")
	}
	if cfg.Redis != "" {
		if err := processrun.ValidateRedis(cfg.Redis); err != nil {
			return err
		}
	}
	host, _, err := net.SplitHostPort(cfg.HTTP)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("demo HTTP address must be literal loopback")
	}
	if cfg.LocalNetwork != "" {
		return validateLocalAddress(cfg.LocalNetwork)
	}
	return nil
}

func configureFlags(flags *flag.FlagSet, cfg *config) {
	flags.StringVar(&cfg.AttachControl, "attach-control", "", "existing control base URL; starts no cluster processes")
	flags.StringVar(&cfg.AttachAuthFile, "attach-auth-file", "", "private API credential file (current API has no credential authentication)")
	flags.StringVar(&cfg.FaultHook, "fault-hook", "", "executable for attach kill and replacement")
	cfg.ActionTimeout = 90 * time.Second
	flags.Func("action-timeout", "deadline including takeover and replacement (default 90s)", func(value string) error {
		timeout, err := time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return errors.New("-action-timeout must be positive")
		}
		cfg.ActionTimeout = timeout
		return nil
	})
	flags.StringVar(&cfg.ResultsDir, "results-dir", "", "create-only results upload directory")
	cfg.RelayBuffer = "on"
	flags.Func("relay-buffer", "relay caller SRTP buffer: on or off (default on)", func(mode string) error {
		if mode != "on" && mode != "off" {
			return errors.New("-relay-buffer must be on or off")
		}
		cfg.RelayBuffer = mode
		return nil
	})
	flags.StringVar(&cfg.Agent, "agent", "echo", "worker audio agent: echo or demo")
	flags.StringVar(&cfg.Bin, "bin", "bin", "directory containing built relay, worker and control")
	flags.StringVar(&cfg.HTTP, "http", "127.0.0.1:9101", "literal loopback page address")
	flags.StringVar(&cfg.LocalNetwork, "local-network", "", "opt in to HTTPS and public media on this literal local IPv4 address; uses the -http port, private APIs remain on loopback")
	flags.StringVar(&cfg.Redis, "redis", "", "explicit dedicated Redis address; otherwise start throwaway Redis (6379 refused)")
	flags.StringVar(&cfg.RedisBinary, "redis-server", "redis-server", "throwaway Redis executable")
}

func run() error {
	var cfg config
	configureFlags(flag.CommandLine, &cfg)
	flag.Parse()
	// Validate exposure and Redis before starting any child or dialing any store.
	if err := validateConfig(cfg); err != nil {
		return err
	}
	listener, fingerprint, err := listenPage(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	manager := &clusterprocess.Manager{}
	ctx, stop := manager.Context()
	defer stop()
	d, err := launch(ctx, manager, cfg)
	if err != nil {
		return err
	}
	files, err := fs.Sub(web, "web")
	if err != nil {
		return err
	}
	scheme := "http"
	if cfg.LocalNetwork != "" {
		scheme = "https"
		fmt.Printf("WARNING: demo page, call controls and relay media are reachable on the local network at %s; use a trusted network. Private APIs remain on loopback.\n", cfg.LocalNetwork)
		fmt.Printf("certificate SHA-256 fingerprint: %s\n", fingerprint)
		fmt.Printf("certificate SHA-256 fingerprint (Chrome): %s\n", chromeFingerprint(fingerprint))
	}
	pageURL := scheme + "://" + listener.Addr().String()
	processrun.Ready(map[string]any{"http": pageURL, "media": d.relay, "redis": d.redis, "certificate_sha256": fingerprint})
	fmt.Printf("cluster demo: %s\n", demoPageURL(cfg, listener.Addr().String(), d.launchToken))
	err = processrun.Serve(ctx, listener, d.handler(http.FileServer(http.FS(files))))
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readyChild(ctx context.Context, child *clusterprocess.Child) (clusterprocess.Ready, error) {
	ready, err := child.Ready(ctx)
	if err != nil {
		child.Stop()
	}
	return ready, err
}
