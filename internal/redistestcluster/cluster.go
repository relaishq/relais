// Package redistestcluster owns disposable Redis Cluster processes for opt-in tests.
package redistestcluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/redistestenv"
)

// Addresses starts three primaries and three replicas when RELAIS_TEST_REDIS_CLUSTER=1.
// An explicit address list instead uses the caller's dedicated cluster. Short tests skip.
func Addresses(t *testing.T) []string {
	t.Helper()
	if testing.Short() {
		t.Skip("Redis Cluster integration is excluded from short mode")
	}
	if csv := redistestenv.ClusterAddresses(t); csv != "" {
		addrs := strings.Split(csv, ",")
		for i := range addrs {
			addrs[i] = strings.TrimSpace(addrs[i])
		}
		return addrs
	}
	if os.Getenv("RELAIS_TEST_REDIS_CLUSTER") != "1" {
		t.Skip("set RELAIS_TEST_REDIS_CLUSTER=1 to start a disposable local cluster")
	}
	return Start(t)
}

// Start always owns its six processes; no existing Redis is contacted.
func Start(t *testing.T) []string {
	t.Helper()
	return StartWithOptions(t, Options{})
}

// Options supplies test-owned TLS material and ACL credentials. Server secrets
// are written only to a private temporary config; redis-cli reads its password
// from REDISCLI_AUTH. Empty settings select plaintext without authentication.
type Options struct {
	CAFile, CertFile, KeyFile string
	Username, Password        string
}

func StartWithOptions(t *testing.T, options Options) []string {
	t.Helper()
	started := time.Now()
	for _, binary := range []string{"redis-server", "redis-cli"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatal(err)
		}
	}
	dir := Directory(t, "nodes")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cleanupStaleServers(ctx); err != nil {
		t.Fatal(err)
	}
	clientOptions := redis.Options{Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, DialerRetries: 1, PipelinePoolSize: -1, Username: options.Username, Password: options.Password, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second}
	if options.CAFile != "" {
		roots := x509.NewCertPool()
		data, err := os.ReadFile(options.CAFile)
		if err != nil {
			t.Fatal(err)
		}
		if !roots.AppendCertsFromPEM(data) {
			t.Fatal("invalid test CA")
		}
		pair, err := tls.LoadX509KeyPair(options.CertFile, options.KeyFile)
		if err != nil {
			t.Fatal(err)
		}
		clientOptions.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{pair}}
	}
	var addrs []string
	var children []*clusterprocess.Child
	var records []string
	t.Cleanup(func() {
		if t.Failed() {
			for i := range children {
				data, _ := os.ReadFile(filepath.Join(dir, fmt.Sprint(i), "redis.log"))
				t.Logf("Redis node %d: %s", i, data)
			}
		}
		for i := len(children) - 1; i >= 0; i-- {
			children[i].Stop()
			if i < len(records) {
				_ = os.Remove(records[i])
			}
		}
	})
	// Reserve all client and bus ports until each child is about to bind them.
	var listeners []net.Listener
	t.Cleanup(func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	})
	for range 12 {
		addr, err := clusterprocess.FreeTCP()
		if err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
	}
	for i := range 6 {
		nodeDir := filepath.Join(dir, fmt.Sprint(i))
		if err := os.Mkdir(nodeDir, 0700); err != nil {
			t.Fatal(err)
		}
		addr := listeners[2*i].Addr().String()
		_, port, _ := net.SplitHostPort(addr)
		_, bus, _ := net.SplitHostPort(listeners[2*i+1].Addr().String())
		_ = listeners[2*i].Close()
		_ = listeners[2*i+1].Close()
		config := fmt.Sprintf("bind 127.0.0.1\nport %s\ncluster-port %s\ncluster-enabled yes\ncluster-config-file nodes.conf\ncluster-node-timeout 1000\ncluster-announce-ip 127.0.0.1\nrepl-diskless-sync-delay 0\nsave \"\"\nappendonly no\ndir %s\n", port, bus, strconv.Quote(nodeDir))
		if options.CAFile != "" {
			config += fmt.Sprintf("port 0\ntls-port %s\ntls-cluster yes\ntls-replication yes\ntls-cert-file %s\ntls-key-file %s\ntls-ca-cert-file %s\ntls-auth-clients yes\n", port, strconv.Quote(options.CertFile), strconv.Quote(options.KeyFile), strconv.Quote(options.CAFile))
		}
		if options.Username != "" {
			config += fmt.Sprintf("user default off\nuser %s on >%s ~* &* +@all\nmasteruser %s\nmasterauth %s\n", options.Username, options.Password, options.Username, options.Password)
		}
		pidFile := filepath.Join(nodeDir, "redis.pid")
		config += "pidfile " + strconv.Quote(pidFile) + "\n"
		configFile := filepath.Join(nodeDir, "redis.conf")
		if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		child, err := clusterprocess.StartSupervised(nodeDir, "redis", UTF8Environment(t), "redis-server", configFile)
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
		watchServer(t, child)
		record, err := recordServer(child, pidFile)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		addrs = append(addrs, addr)
		if err := child.WaitLog(ctx, "Ready to accept connections"); err != nil {
			data, _ := os.ReadFile(filepath.Join(nodeDir, "redis.log"))
			text := string(data)
			if options.CAFile != "" && (strings.Contains(strings.ToLower(text), "tls support not compiled") || strings.Contains(strings.ToLower(text), "tls support is not compiled")) {
				if os.Getenv("RELAIS_TEST_REDIS_TLS_REQUIRE") == "1" {
					t.Fatal("Redis TLS support required")
				}
				t.Skip("local redis-server has no TLS support; required in CI")
			}
			t.Fatalf("Redis startup: %v: %s", err, text)
		}
	}
	args := append([]string{"--cluster", "create"}, addrs...)
	args = append(args, "--cluster-replicas", "1", "--cluster-yes")
	if options.CAFile != "" {
		args = append([]string{"--tls", "--cacert", options.CAFile, "--cert", options.CertFile, "--key", options.KeyFile}, args...)
	}
	if options.Username != "" {
		args = append([]string{"--user", options.Username}, args...)
	}
	command := exec.CommandContext(ctx, "redis-cli", args...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "REDISCLI_AUTH=") {
			command.Env = append(command.Env, entry)
		}
	}
	if options.Password != "" {
		command.Env = append(command.Env, "REDISCLI_AUTH="+options.Password)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cluster create: %v: %s", err, output)
	}
	for _, addr := range addrs {
		opts := clientOptions
		opts.Addr = addr
		client := redis.NewClient(&opts)
		for {
			info, err := client.ClusterInfo(ctx).Result()
			replication, replErr := client.Info(ctx, "replication").Result()
			replicaReady := strings.Contains(replication, "role:slave") && strings.Contains(replication, "master_link_status:up")
			if strings.Contains(replication, "role:master") && strings.Contains(replication, "connected_slaves:1") {
				id, idErr := client.Do(ctx, "CLUSTER", "MYID").Text()
				replicas, replicasErr := client.Do(ctx, "CLUSTER", "REPLICAS", id).StringSlice()
				// A replica can finish its link before the primary's cluster gossip knows
				// its role. Failover tests need both views settled, not just TCP readiness.
				replicaReady = idErr == nil && replicasErr == nil && len(replicas) == 1
			}
			if err == nil && replErr == nil && strings.Contains(info, "cluster_state:ok") && strings.Contains(info, "cluster_known_nodes:6") && strings.Contains(info, "cluster_size:3") && replicaReady {
				break
			}
			select {
			case <-ctx.Done():
				_ = client.Close()
				t.Fatalf("cluster readiness %s: %v %s %s", addr, err, info, replication)
			case <-time.After(25 * time.Millisecond):
			}
		}
		_ = client.Close()
	}
	t.Logf("disposable 3-primary/3-replica cluster ready in %s", time.Since(started))
	var ports []string
	for _, listener := range listeners {
		ports = append(ports, listener.Addr().String())
	}
	t.Logf("owned Redis client/bus ports: %v", ports)
	return addrs
}
