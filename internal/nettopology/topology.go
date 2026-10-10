package nettopology

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type Topology struct {
	Plan     Plan
	mu       sync.Mutex
	closed   bool
	closeErr error
	runner   func(context.Context, Command) (string, error)
	logf     func(string, ...any)
}

func (t *Topology) execute(ctx context.Context, commands []Command) error {
	for _, command := range commands {
		if _, err := t.runner(ctx, command); err != nil {
			return err
		}
	}
	return nil
}
func (t *Topology) AllowMedia(ctx context.Context, addr netip.AddrPort) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("topology is closed")
	}
	commands, err := t.Plan.MediaCommands(addr)
	if err != nil {
		return err
	}
	return t.execute(ctx, commands)
}

// VerifyPrivateAPIs first proves every private listener is live from the
// driver, then attempts those same TCP endpoints from the caller namespace.
// The real call separately proves the allowed UDP media path.
func (t *Topology) VerifyPrivateAPIs(ctx context.Context, addresses []string) error {
	for _, addr := range addresses {
		endpoint, err := netip.ParseAddrPort(addr)
		if err != nil || endpoint.Port() == 6379 || !t.Plan.Datacentre.Contains(endpoint.Addr()) {
			return fmt.Errorf("invalid private isolation endpoint %q", addr)
		}
		d := net.Dialer{Timeout: time.Second}
		conn, err := d.DialContext(ctx, "tcp4", addr)
		if err != nil {
			return fmt.Errorf("private listener %s is not live: %w", addr, err)
		}
		_ = conn.Close()
		err = withNamespace(t.Plan.Roles["caller"].Namespace, func() error {
			d := net.Dialer{Timeout: 150 * time.Millisecond}
			conn, err := d.DialContext(ctx, "tcp4", addr)
			if err == nil {
				_ = conn.Close()
				return fmt.Errorf("caller reached private API %s", addr)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Close is idempotent and uses a fresh context: canceled runs still clean up.
// Caller sockets and managed children must close before this method.
func (t *Topology) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return t.closeErr
	}
	t.closed = true
	if t.logf != nil {
		t.logf("cleanup topology %s", t.Plan.ID)
	}
	var failures []error
	for _, link := range t.Plan.Links {
		failures = append(failures, t.removeLink(link.HostLink), t.removeLink(link.PeerLink))
	}
	failures = append(failures, t.removeLink(t.Plan.ManagementHost), t.removeLink(t.Plan.ManagementPeer))
	// Reverse deterministic setup order, and include partially created veths.
	seen := map[string]bool{}
	for i := len(t.Plan.Links) - 1; i >= 0; i-- {
		ns := t.Plan.Roles[t.Plan.Links[i].Role].Namespace
		if !seen[ns] {
			failures = append(failures, t.removeNamespace(ns))
			seen[ns] = true
		}
	}
	failures = append(failures, t.removeNamespace(t.Plan.FabricNamespace))
	t.closeErr = errors.Join(failures...)
	return t.closeErr
}
func (t *Topology) cleanupCommand(command Command) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return t.runner(ctx, command)
}
func (t *Topology) removeLink(name string) error {
	if output, err := t.cleanupCommand(Command{"ip", "link", "show", "dev", name}); err != nil {
		if strings.Contains(output, "does not exist") {
			return nil
		}
		return err
	}
	_, err := t.cleanupCommand(Command{"ip", "link", "delete", name})
	return err
}
