package room

import (
	"encoding/binary"
	"net"
	"time"
)

// UDP audio protocol: uint16 session-ID length, ID, uint64 request sequence,
// Opus. Reply echoes the request sequence, followed by the peer's latest Opus.
// Only the two current joined sessions are retained. Packets expire at 100ms.
func (r *Room) ServeAudio(conn *net.UDPConn) error {
	type packet struct {
		data []byte
		at   time.Time
	}
	latest := map[string]packet{}
	buf := make([]byte, 1501)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		if n < 10 || n > 1500 {
			continue
		}
		size := int(binary.BigEndian.Uint16(buf[:2]))
		if size == 0 || size > 128 || n < 10+size {
			continue
		}
		id := string(buf[2 : 2+size])
		seq := buf[2+size : 10+size]
		r.mu.Lock()
		own, peer := "", ""
		active := map[string]bool{}
		for _, p := range r.identities {
			if p.player.Joined && p.session != "" {
				active[p.session] = true
				if p.session == id {
					own = id
				} else {
					peer = p.session
				}
			}
		}
		r.mu.Unlock()
		for key := range latest {
			if !active[key] {
				delete(latest, key)
			}
		}
		if own == "" {
			continue
		}
		latest[id] = packet{append([]byte(nil), buf[10+size:n]...), time.Now()}
		response := append([]byte(nil), seq...)
		if p, ok := latest[peer]; ok && time.Since(p.at) < 100*time.Millisecond {
			response = append(response, p.data...)
		}
		_, _ = conn.WriteToUDP(response, addr)
	}
}
