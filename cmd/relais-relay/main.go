package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/relais/internal/processidentity"
	"github.com/relais/internal/processrun"
	"github.com/relais/internal/relaylease"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

func run() error {
	var storeConfig processrun.StoreConfig
	storeConfig.Flags(flag.CommandLine)
	public := flag.String("media", processrun.Env("RELAIS_RELAY_MEDIA", "127.0.0.1:0"), "public UDP address")
	leg := flag.String("leg", processrun.Env("RELAIS_RELAY_LEG", "127.0.0.1:0"), "private worker-leg UDP address")
	httpAddr := flag.String("http", processrun.Env("RELAIS_RELAY_HTTP", "127.0.0.1:0"), "private control HTTP address")
	hold := flag.Duration("hold-timeout", 3*time.Second, "abandoned move backstop")
	restoreOff := flag.Bool("route-restore-off", false, "disable persisted-route restore for baseline measurement")
	restoreTimeout := flag.Duration("route-restore-timeout", 5*time.Second, "startup route restore budget; continue with partial results on expiry")
	maxFlows := flag.Int("max-flows", 65536, "maximum confirmed and pending caller routes")
	standby := flag.Bool("standby", false, "wait without binding any sockets, then fence and take over")
	leaseKey := flag.String("relay-lease", "default", "same-host relay lease key; shared by active and standby")
	leaseTTL := flag.Duration("relay-lease-ttl", relaylease.DefaultTTL, "relay lease lifetime")
	leaseRenew := flag.Duration("relay-lease-renew", relaylease.DefaultRenew, "relay lease renewal interval")
	leasePoll := flag.Duration("relay-lease-poll", relaylease.DefaultPoll, "standby claim polling interval")
	flag.Parse()
	if err := validateAddresses(*standby, *public, *leg, *httpAddr); err != nil {
		return err
	}
	ctx, cancel := processrun.Context()
	defer cancel()
	store, err := storeConfig.OpenOwners(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	identity, err := processidentity.Current()
	if err != nil {
		return err
	}
	process := sessionstore.RelayProcess{Owner: rand.Text(), PID: identity.PID, Start: identity.Start}
	if *standby {
		log.Printf("STANDBY_WAIT pid=%d lease=%s", os.Getpid(), *leaseKey)
	}
	guard, err := relaylease.Acquire(ctx, relaylease.Config{Store: store, Key: *leaseKey, Process: process, TTL: *leaseTTL, Renew: *leaseRenew, Poll: *leasePoll})
	if err != nil {
		return err
	}
	defer guard.Close()
	timing := guard.Timing()
	continuity := &relay.ContinuityStatus{Epoch: guard.Lease().Epoch, ClaimAt: timing.ClaimAt, Wait: timing.Wait, Fence: timing.Fence, Activate: timing.Activate, FencingFailures: timing.FencingFailures, StoreFailures: timing.StoreFailures}
	bindAt := time.Now()
	r, err := relay.New(relay.Config{InstanceID: process.Owner, ForwardingAllowed: guard.Allowed, Continuity: continuity, PublicAddr: *public, WorkerAddr: *leg, Owners: store, Routes: store, DisableRouteRestore: *restoreOff, RouteRestoreTimeout: *restoreTimeout, MaxFlows: *maxFlows, HoldTimeout: *hold})
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	guard.OnLost(r.Fence)
	if !guard.Allowed() {
		return sessionstore.ErrLeaseLost
	}
	listener, err := processrun.ListenPrivate(*httpAddr)
	if err != nil {
		return err
	}
	continuity.BindRestore = time.Since(bindAt)
	processrun.Ready(map[string]any{"instance": process.Owner, "epoch": guard.Lease().Epoch, "http": "http://" + listener.Addr().String(), "media": r.PublicAddr(), "leg": r.WorkerAddr()})
	err = processrun.Serve(guard.Context(), listener, r.PrivateHandler())
	if lost := guard.Err(); lost != nil {
		return lost
	}
	return err
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// Reject invalid socket configuration before a candidate can claim or signal.
// Validation itself must not bind anything, including the private HTTP socket.
func validateAddresses(standby bool, public, leg, httpAddr string) error {
	for _, addr := range []string{public, leg} {
		udp, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return err
		}
		if udp.IP == nil || udp.IP.IsUnspecified() {
			return fmt.Errorf("relay UDP address must name a specific IP: %s", addr)
		}
		if standby && udp.Port == 0 {
			return fmt.Errorf("standby requires fixed socket addresses: %s", addr)
		}
	}
	host, port, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("private HTTP listener must use a literal loopback IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || standby && n == 0 {
		return fmt.Errorf("invalid relay HTTP port: %s", httpAddr)
	}
	return nil
}
