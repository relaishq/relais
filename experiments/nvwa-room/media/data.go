package media

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
)

// DC is the stage-2 integration seam. It negotiates the frozen control/state
// labels through relais-signal; no standalone Pion server is substituted.
type DC struct {
	control, state *webrtc.DataChannel
	incoming       chan []byte
	done           chan struct{}
	once           sync.Once
}

func NewDC(pc *webrtc.PeerConnection) (*DC, error) {
	return NewDCPolicy(pc, false)
}

// NewDCPolicy changes only the state channel; control is always reliable.
func NewDCPolicy(pc *webrtc.PeerConnection, reliableState bool) (*DC, error) {
	d := &DC{incoming: make(chan []byte, 512), done: make(chan struct{})}
	var err error
	d.control, err = pc.CreateDataChannel("control", nil)
	if err != nil {
		return nil, err
	}
	ordered := false
	retries := uint16(0)
	stateInit := &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retries}
	if reliableState {
		stateInit = nil
	}
	d.state, err = pc.CreateDataChannel("state", stateInit)
	if err != nil {
		return nil, err
	}
	for _, channel := range []*webrtc.DataChannel{d.control, d.state} {
		channel.OnClose(func() { _ = d.Close() })
		channel.OnMessage(func(m webrtc.DataChannelMessage) {
			if len(m.Data) > room.MaxMessage {
				_ = d.Close()
				return
			}
			b := append([]byte(nil), m.Data...)
			select {
			case d.incoming <- b:
			case <-d.done:
			default:
				_ = d.Close()
			}
		})
	}
	return d, nil
}
func (d *DC) Send(b []byte) error {
	if len(b) > room.MaxMessage {
		return errors.New("message_too_large")
	}
	channel := d.control
	var m room.Message
	if json.Unmarshal(b, &m) == nil && m.Type == "state" {
		channel = d.state
	}
	deadline := time.Now().Add(5 * time.Second)
	for channel.ReadyState() != webrtc.DataChannelStateOpen {
		select {
		case <-d.done:
			return errors.New("datachannel_closed")
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("datachannel_open_timeout")
		}
	}
	if channel.BufferedAmount()+uint64(len(b)) > room.MaxPending {
		return errors.New("datachannel_pending_limit")
	}
	return channel.Send(b)
}
func (d *DC) Receive() ([]byte, error) {
	select {
	case b := <-d.incoming:
		return b, nil
	case <-d.done:
		return nil, errors.New("datachannel_closed")
	}
}
func (d *DC) Close() error {
	d.once.Do(func() { close(d.done); _ = d.control.Close(); _ = d.state.Close() })
	return nil
}
