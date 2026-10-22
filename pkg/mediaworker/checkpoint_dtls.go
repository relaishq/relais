package mediaworker

import (
	"encoding/binary"
	"fmt"
)

// armDTLSBound runs under session.mu before the first write is sent. Refuse
// plaintext setup: its exempt epoch cannot bound subsequent encrypted records.
func (c *sessionCheckpoint) armDTLSBound(next dtlsSequenceCheckpoint) error {
	if next.epoch == 0 {
		return fmt.Errorf("%w: epoch %d", errDTLSCheckpointEpoch, next.epoch)
	}
	if c.dtlsProvisional == nil && c.dtlsAcknowledged == nil {
		c.dtlsProvisional = &next
	}
	return nil
}

// allowDTLSLocked runs under session.mu, at the last point before a caller
// datagram can leave the worker. A coalesced datagram is all-or-nothing: every
// record must fit. No caller bytes are sent when any record cannot be parsed.
func (c *sessionCheckpoint) allowDTLSLocked(p []byte) bool {
	bound := c.dtlsAcknowledged
	if bound == nil {
		bound = c.dtlsProvisional
	}
	if bound == nil {
		return true // no checkpoint write has been attempted
	}
	// Invariant: every encrypted record this owner sends has
	// sequence < N_c + R for every checkpoint c that could be latest at takeover.
	// The first capture arms the provisional bound under this same lock;
	// only successful writes advance it. Serialized captures are monotone,
	// so a lost reply can only leave a stricter bound than the committed one.
	for len(p) > 0 {
		const headerBytes = 13 // DTLS 1.2, no connection ID negotiated by this worker
		if len(p) < headerBytes || !isDTLS(p) || p[0] == 25 {
			return c.dropDTLSDatagram()
		}
		size := headerBytes + int(binary.BigEndian.Uint16(p[11:13]))
		if size > len(p) {
			return c.dropDTLSDatagram()
		}
		epoch := binary.BigEndian.Uint16(p[3:5])
		sequence := uint64(binary.BigEndian.Uint16(p[5:7]))<<32 | uint64(binary.BigEndian.Uint32(p[7:11]))
		// Epoch zero is plaintext and has no AEAD nonce to reuse. Pion
		// coalesces its retransmitted ChangeCipherSpec with encrypted Finished;
		// allowing it is necessary to complete a handshake after packet loss.
		// Only the checkpointed encrypted epoch is supported; a new keyed epoch
		// would require an explicitly supported rekey protocol.
		if epoch != 0 && (epoch != bound.epoch || sequence >= bound.next+dtlsSequenceReservation) {
			return c.dropDTLSDatagram()
		}
		p = p[size:]
	}
	return true
}

func (c *sessionCheckpoint) dropDTLSDatagram() bool {
	s := c.session
	s.worker.metrics.dtlsSequenceDrops.Add(1)
	if !c.dtlsCheckpointRequested {
		c.dtlsCheckpointRequested = true
		c.request()
	}
	return false
}
