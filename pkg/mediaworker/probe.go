package mediaworker

import (
	"errors"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relaishq/relais/internal/workerprobe"
)

// captureZombie is reachable only through workerprobe.Enable and its
// opt-in internal registry, which registers relay-mode workers only.
// It copies the old sender's keys and counters without touching its live
// contexts. The returned sender uses the actual old worker's private socket.
func (w *Worker) captureZombie(id string) (workerprobe.Sender, error) {
	s := w.session(id)
	if s == nil {
		return nil, ErrUnknownSession
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dtlsConn == nil || s.state.Audio.Packets == 0 {
		return nil, ErrNotEstablished
	}

	state, ok := s.dtlsConn.ConnectionState()
	if !ok {
		return nil, errors.New("mediaworker: probe needs DTLS state")
	}

	cfg := srtp.Config{Profile: s.state.SRTP.Profile}
	if err := cfg.ExtractSessionKeysFromDTLS(&state, false); err != nil {
		return nil, err
	}

	out, err := srtp.CreateContext(cfg.Keys.LocalMasterKey, cfg.Keys.LocalMasterSalt, cfg.Profile)
	if err != nil {
		return nil, err
	}

	track := s.state.Audio
	if err := restoreOutboundIndex(out, track.SSRC, track.HighestSentIndex); err != nil {
		return nil, err
	}

	addr := s.state.ICE.RemoteAddr
	leg := s.currentLeg()
	if s.revokedRelayLegs[leg] {
		return nil, ErrRelayUnsupported
	}
	slot, epoch := "", uint64(0)
	if s.state.ICE.Relay != nil {
		slot = s.state.ICE.Relay.ID
		w.mu.Lock()
		epoch = w.relayEpochs[slot]
		cutoff := w.relayCutoffs[slot]
		w.mu.Unlock()
		if epoch == 0 || epoch <= cutoff {
			return nil, ErrRelayUnsupported
		}
	}
	seq := uint16(track.HighestSentIndex) //nolint:gosec // RTP sequence is the low 16 bits
	timestamp := track.LastTimestamp
	var mu sync.Mutex
	return func(marker []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		seq++
		timestamp += 960
		pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: track.PayloadType, SSRC: track.SSRC, SequenceNumber: seq, Timestamp: timestamp}, Payload: marker}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}

		encrypted, err := out.EncryptRTP(nil, raw, nil)
		if err != nil {
			return nil, err
		}

		w.mu.Lock()
		cutoff := w.relayCutoffs[slot]
		w.mu.Unlock()
		if slot != "" && epoch <= cutoff {
			return nil, ErrRelayUnsupported
		}
		_, err = w.sendVia(encrypted, addr, leg)
		return encrypted, err
	}, nil
}
