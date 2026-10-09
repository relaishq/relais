// Package relayleg carries trusted drain barriers on the private UDP leg.
// A worker acknowledges a barrier only after its read loop has finished
// preceding media. These datagrams never travel on the public caller leg.
package relayleg

import "encoding/binary"

const barrierSize = 13

// Barrier makes a request or acknowledgement for one bounded relay hold.
func Barrier(id uint64, ack bool) []byte {
	packet := make([]byte, barrierSize)
	copy(packet, []byte{'R', 'L', 'S', 1})
	if ack {
		packet[4] = 1
	}
	binary.BigEndian.PutUint64(packet[5:], id)
	return packet
}

// ParseBarrier recognizes only the private control datagram, not a wrapped
// caller packet. The receiver must also check the datagram's source address.
func ParseBarrier(packet []byte) (id uint64, ack bool, ok bool) {
	if len(packet) != barrierSize || string(packet[:4]) != "RLS\x01" || packet[4] > 1 {
		return 0, false, false
	}

	return binary.BigEndian.Uint64(packet[5:]), packet[4] == 1, true
}
