package nettopology

import (
	"bytes"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func profileProbeRTT(caller net.PacketConn, endpoint net.Addr, probe []byte) (time.Duration, error) {
	if err := caller.SetDeadline(time.Now().Add(350 * time.Millisecond)); err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := caller.WriteTo(probe, endpoint); err != nil {
		return 0, err
	}
	var b [100]byte
	for {
		n, addr, err := caller.ReadFrom(b[:])
		if err != nil {
			return 0, err
		}
		if addr.String() == endpoint.String() && bytes.Equal(b[:n], probe) {
			return time.Since(start), nil
		}
	}
}

func profileMedianRTT(samples []time.Duration) time.Duration {
	ordered := slices.Clone(samples)
	slices.Sort(ordered)
	mid := len(ordered) / 2
	if len(ordered)%2 == 0 {
		return (ordered[mid-1] + ordered[mid]) / 2
	}
	return ordered[mid]
}

type profileProbeConn struct {
	net.PacketConn
	read        func([]byte) (int, net.Addr, error)
	write       func([]byte, net.Addr) (int, error)
	setDeadline func(time.Time) error
}

func (c profileProbeConn) ReadFrom(b []byte) (int, net.Addr, error) { return c.read(b) }
func (c profileProbeConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	return c.write(b, addr)
}
func (c profileProbeConn) SetDeadline(deadline time.Time) error { return c.setDeadline(deadline) }

func TestProfileProbeIgnoresStaleAndMismatchedReplies(t *testing.T) {
	endpoint := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9000}
	other := &net.UDPAddr{IP: endpoint.IP, Port: 9001}
	probe := []byte("region/1")
	type reply struct {
		payload string
		addr    net.Addr
	}
	for _, tc := range []struct {
		name    string
		matched bool
	}{{"matched", true}, {"timeout", false}} {
		t.Run(tc.name, func(t *testing.T) {
			replies := []reply{
				{"region/0", endpoint},
				{"far-caller/1", endpoint},
				{"region/1-extra", endpoint},
				{"region/1", other},
			}
			if tc.matched {
				replies = append(replies, reply{string(probe), endpoint})
			}
			reads, deadlines := 0, 0
			conn := profileProbeConn{
				read: func(b []byte) (int, net.Addr, error) {
					if reads == len(replies) {
						return 0, nil, os.ErrDeadlineExceeded
					}
					reply := replies[reads]
					reads++
					return copy(b, reply.payload), reply.addr, nil
				},
				write: func(b []byte, addr net.Addr) (int, error) {
					require.Equal(t, probe, b)
					require.Equal(t, endpoint, addr)
					return len(b), nil
				},
				setDeadline: func(time.Time) error { deadlines++; return nil },
			}
			rtt, err := profileProbeRTT(conn, endpoint, probe)
			if tc.matched {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, os.ErrDeadlineExceeded)
				require.Zero(t, rtt, "unmatched replies must not produce a sample")
			}
			require.Equal(t, len(replies), reads)
			require.Equal(t, 1, deadlines, "ignored replies must not extend the deadline")
		})
	}
}

func TestProfileMedianResistsOutliersAndPreservesRawSamples(t *testing.T) {
	for _, tc := range []struct {
		samples []time.Duration
		want    time.Duration
	}{
		{[]time.Duration{61, 20, 21, 19, 5}, 20},
		{[]time.Duration{61, 20, 22, 19, 22, 5}, 21},
	} {
		raw := slices.Clone(tc.samples)
		require.Equal(t, tc.want, profileMedianRTT(tc.samples))
		require.Equal(t, raw, tc.samples)
	}
}
