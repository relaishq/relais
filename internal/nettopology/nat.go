package nettopology

import (
	"errors"
	"net"
	"net/netip"
	"sync"

	"github.com/relaishq/relais/internal/relayleg"
)

// NATMapping is a loopback UDP fault seam for process and in-process tests.
// The worker-facing socket stays fixed while Rebind replaces its relay-facing
// mapping and removes the old return path. It never modifies media packets.
type NATMapping struct {
	mu            sync.Mutex
	front, back   *net.UDPConn
	relay, worker netip.AddrPort
	binding       []byte
	closed        bool
	done          sync.WaitGroup
}

func NewNATMapping(relay netip.AddrPort) (*NATMapping, error) {
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	n := &NATMapping{front: front, relay: relay}
	if err := n.Rebind(netip.AddrPort{}); err != nil {
		_ = front.Close()
		return nil, err
	}
	n.done.Go(func() {
		b := make([]byte, 65536)
		for {
			size, from, err := front.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.worker = from
			back := n.back
			if kind, _, _, ok := relayleg.ParseBinding(b[:size]); ok && (kind == relayleg.Hello || kind == relayleg.Keepalive) {
				n.binding = append(n.binding[:0], b[:size]...)
			}
			n.mu.Unlock()
			_, _ = back.WriteToUDPAddrPort(b[:size], relay)
		}
	})
	return n, nil
}
func (n *NATMapping) WorkerAddr() netip.AddrPort {
	return n.front.LocalAddr().(*net.UDPAddr).AddrPort()
}
func (n *NATMapping) ObservedAddr() netip.AddrPort {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.back.LocalAddr().(*net.UDPAddr).AddrPort()
}
func (n *NATMapping) Rebind(address netip.AddrPort) error {
	if !address.IsValid() {
		address = netip.MustParseAddrPort("127.0.0.1:0")
	}
	back, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(address))
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		_ = back.Close()
		return net.ErrClosed
	}
	old := n.back
	n.back = back
	if old != nil {
		_ = old.Close()
	}
	n.done.Go(func() {
		b := make([]byte, 65536)
		for {
			size, _, err := back.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			n.mu.Lock()
			worker := n.worker
			n.mu.Unlock()
			if worker.IsValid() {
				_, _ = n.front.WriteToUDPAddrPort(b[:size], worker)
			}
		}
	})
	return nil
}

// Observe replays the worker's latest leg hello/keepalive from the current
// mapping. Tests use it to align a rebind with an already gated command.
func (n *NATMapping) Observe() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.binding) == 0 {
		return errors.New("NAT mapping has not received a leg hello")
	}
	_, err := n.back.WriteToUDPAddrPort(n.binding, n.relay)
	return err
}
func (n *NATMapping) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	err := errors.Join(n.front.Close(), n.back.Close())
	n.mu.Unlock()
	n.done.Wait()
	return err
}
