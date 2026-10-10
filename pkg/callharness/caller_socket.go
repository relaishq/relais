package callharness

import (
	"errors"
	"net"
)

func (h *Harness) newCallerSocket(rec *recorder) (*callerSocket, error) {
	if h.external == nil || h.external.CallerSocket == nil {
		return newCallerSocket(rec)
	}
	conn, err := h.external.CallerSocket()
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("callharness: caller socket factory returned nil")
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr == nil || addr.IP.To4() == nil || addr.IP.IsUnspecified() || addr.Port == 0 {
		_ = conn.Close()
		return nil, errors.New("callharness: caller socket must bind a specific IPv4 UDP address")
	}
	socket := wrapCallerSocket(rec, conn)
	socket.candidateIP = append(net.IP(nil), addr.IP...)
	return socket, nil
}

func (s *callerSocket) acceptsCandidate(ip net.IP) bool {
	if s.candidateIP == nil {
		return ip.IsLoopback()
	}
	return s.candidateIP.Equal(ip)
}
