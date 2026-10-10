// relais-demo owns a loopback browser demo and its separate media processes.
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

type config struct{ Bin, HTTP, Redis, RedisBinary string }

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
	_, relay, err := start(ctx, "relay", processEnv("", addr, prefix, false), filepath.Join(bin, "relais-relay"), "-media", "127.0.0.1:0", "-leg", "127.0.0.1:0", "-http", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	controlAddr, err := clusterprocess.FreeTCP()
	if err != nil {
		return nil, err
	}
	d := &demo{ctx: ctx, client: &http.Client{Timeout: 10 * time.Second}, control: "http://" + controlAddr, relay: relay.Media, redis: addr, dir: dir, workers: map[string]*workerProcess{}}
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
		child, ready, err := start(ctx, "worker-"+name, workerEnv, filepath.Join(bin, "relais-worker"), "-media", media, "-http", "127.0.0.1:0", "-name", name, "-control", d.control, "-relay-leg", relay.Leg, "-relay-media", relay.Media)
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
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		addr := conn.LocalAddr().String()
		if err := conn.Close(); err != nil {
			return "", err
		}
		if !used[addr] {
			used[addr] = true
			return addr, nil
		}
	}
	return "", errors.New("could not allocate a new worker UDP address")
}

func validateConfig(cfg config) error {
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
	return nil
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.Bin, "bin", "bin", "directory containing built relay, worker and control")
	flag.StringVar(&cfg.HTTP, "http", "127.0.0.1:9101", "literal loopback page address")
	flag.StringVar(&cfg.Redis, "redis", "", "explicit dedicated Redis address; otherwise start throwaway Redis (6379 refused)")
	flag.StringVar(&cfg.RedisBinary, "redis-server", "redis-server", "throwaway Redis executable")
	flag.Parse()
	// Validate exposure and Redis before starting any child or dialing any store.
	if err := validateConfig(cfg); err != nil {
		return err
	}
	listener, err := processrun.ListenPrivate(cfg.HTTP)
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
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String(), "media": d.relay, "redis": d.redis})
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	fmt.Printf("cluster demo: http://localhost:%s/?source=test&autostart=0\n", port)
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
