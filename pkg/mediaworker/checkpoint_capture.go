package mediaworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

type checkpointCaptureKind uint8

const (
	checkpointLive checkpointCaptureKind = iota
	checkpointExact
)

func (c *sessionCheckpoint) capture(kind checkpointCaptureKind, payload *movePayload) ([]byte, error) {
	if kind == checkpointLive {
		return c.snapshotBytes()
	}
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if !s.fenced.Load() && s.ctx.Err() == nil && s.agent != nil {
			s.agent.paused = false
			if s.agent.v2 != nil {
				s.wakePacer()
			}
		}
	}()

	if s.fenced.Load() {
		return nil, errHandedOver
	}
	if s.dtlsConn == nil || s.srtpIn == nil {
		return nil, ErrNotEstablished
	}

	// Fence first, so the DTLS connection cannot send a record the snapshot
	// does not account for.
	s.fenced.Store(true)
	connState, ok := s.dtlsConn.ConnectionState()
	if !ok {
		s.fenced.Store(false)

		return nil, errors.New("mediaworker: DTLS connection state unavailable")
	}
	dtlsState, err := connState.MarshalBinary()
	if err != nil {
		s.fenced.Store(false)

		return nil, fmt.Errorf("mediaworker: export DTLS state: %w", err)
	}
	s.checkpointRates(&s.state.Checkpoint)
	exported := s.state
	if exported.Agent.V2 != nil {
		exported.Agent = cloneAgentState(s.state.Agent)
		now := time.Now()
		if !s.storeClockAt.IsZero() {
			now = s.storeClock.Add(time.Since(s.storeClockAt))
		}
		exported.Agent.V2.Quota = c.captureSnapshotQuotaMode(now, true)
		payload.ControlAck = c.controlAck
	}
	value := snapshot{
		Version:        s.state.Version,
		AgentMoveOnly:  payload,
		State:          exported,
		DTLSConnection: dtlsState,
		split:          c.componentExportState(),
	}
	var state []byte
	if payload != nil && payload.Bridge != nil && value.split != nil {
		// Avoid an outer MarshalJSON buffer around the already encoded component
		// envelope. Keep the exact v10 wire bytes and the legacy capture path.
		state, err = encodeComponentSnapshot(value)
	} else {
		state, err = json.Marshal(value)
	}
	if err != nil {
		s.fenced.Store(false)

		return nil, fmt.Errorf("mediaworker: encode session state: %w", err)
	}

	if payload != nil {
		if err := validateExactWire(state, payload.Bridge != nil); err != nil {
			s.fenced.Store(false)
			return nil, privateapi.Uncertain(err)
		}
	}
	return state, nil
}
