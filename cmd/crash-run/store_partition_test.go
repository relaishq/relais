//go:build !windows

package main

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Partition only one worker's TCP store path. Relay, control and the successor
// keep using the dedicated Redis directly. No Redis process is paused here.
type processStorePartition struct {
	listener    net.Listener
	mu          sync.Mutex
	blocked     bool
	connections map[net.Conn]net.Conn
	done        sync.WaitGroup
}

func newProcessStorePartition(t *testing.T, target string) *processStorePartition {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	p := &processStorePartition{listener: l, connections: make(map[net.Conn]net.Conn)}
	p.done.Go(func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			p.done.Go(func() {
				defer func() { _ = client.Close() }()
				upstream, err := net.DialTimeout("tcp4", target, time.Second)
				if err != nil {
					return
				}
				defer func() { _ = upstream.Close() }()
				p.mu.Lock()
				if p.blocked {
					p.mu.Unlock()
					return
				}
				p.connections[client] = upstream
				p.mu.Unlock()
				defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
				p.done.Go(func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() })
				_, _ = io.Copy(client, upstream)
				_ = client.Close()
			})
		}
	})
	t.Cleanup(func() {
		_ = l.Close()
		p.partition(true)
		finished := make(chan struct{})
		go func() { p.done.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("store proxy did not stop")
		}
	})
	return p
}

func (p *processStorePartition) partition(blocked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = blocked
	if blocked {
		for client, upstream := range p.connections {
			_ = client.Close()
			_ = upstream.Close()
		}
	}
}
