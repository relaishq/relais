// Package audioagent is an opt-in spike-only Opus cross-session adapter.
package audioagent

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/relaishq/relais/pkg/agent"
)

const stateVersion = 0x52415544

func Factory(bus string) (agent.Factory, error) {
	u, err := url.Parse(bus)
	if err != nil || u.Scheme != "udp" || u.Path != "" {
		return nil, errors.New("room-audio requires udp://loopback-IP:port")
	}
	addr, err := net.ResolveUDPAddr("udp", u.Host)
	if err != nil || addr.IP == nil || !addr.IP.IsLoopback() || addr.Port == 0 {
		return nil, errors.New("room-audio bus must be a loopback UDP address")
	}
	return func(id string) agent.Agent { return &Agent{id: id, addr: addr} }, nil
}

type Agent struct {
	id   string
	addr *net.UDPAddr
	seq  uint64
}

func (a *Agent) Process(ctx context.Context, in agent.Input) (agent.Output, error) {
	if len(a.id) > 128 || len(in.Payload) > 1200 {
		return agent.Output{}, nil
	}
	// Per-callback sockets avoid retaining a resource without an Agent.Close hook.
	conn, err := net.DialUDP("udp", nil, a.addr)
	if err != nil {
		return agent.Output{}, nil
	}
	defer conn.Close()
	deadline := time.Now().Add(4 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	a.seq++
	data := make([]byte, 10+len(a.id)+len(in.Payload))
	binary.BigEndian.PutUint16(data, uint16(len(a.id)))
	copy(data[2:], a.id)
	binary.BigEndian.PutUint64(data[2+len(a.id):], a.seq)
	copy(data[10+len(a.id):], in.Payload)
	if _, err = conn.Write(data); err != nil {
		return agent.Output{}, nil
	}
	var response [1500]byte
	n, err := conn.Read(response[:])
	if err != nil || n <= 8 || binary.BigEndian.Uint64(response[:8]) != a.seq {
		return agent.Output{}, nil
	}
	return agent.Output{Audio: append([]byte(nil), response[8:n]...)}, nil
}
func (a *Agent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: stateVersion}, nil
}
func (a *Agent) Restore(_ context.Context, s agent.State) error {
	if s.Version != stateVersion || len(s.Bytes) != 0 {
		return agent.ErrVersion
	}
	return nil
}
func (a *Agent) Resume(context.Context, agent.ResumeNotice) error { return nil }
