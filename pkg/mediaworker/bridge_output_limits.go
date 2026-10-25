package mediaworker

import (
	"fmt"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// BridgeOutputLimits is the bridge host profile. It does not change public v2
// agent declarations or the defaults used by task audio. Zero selects the documented host defaults.
type BridgeOutputLimits struct {
	ActorReserveBytes    int
	ObserverRequestBytes int
	ObserverMailboxBytes int
	AdapterMoveOnlyBytes int
	ExportHeadroomBytes  int
	AdmissionTimeout     time.Duration
	PendingPackets       int
	PendingPayloadBytes  int
	AudioBytes           int
	AudioDuration        time.Duration
	ReceiptRanges        int
	ReceiptBytes         int
}

func (l BridgeOutputLimits) Defaults() BridgeOutputLimits {
	if l.ObserverMailboxBytes == 0 {
		l.ObserverMailboxBytes = 20 << 10
	}
	if l.AdapterMoveOnlyBytes == 0 {
		l.AdapterMoveOnlyBytes = 11 << 10
	}
	if l.ObserverRequestBytes == 0 {
		l.ObserverRequestBytes = 16 << 10
	}
	if l.ActorReserveBytes == 0 {
		l.ActorReserveBytes = 160 << 10
	}
	if l.ExportHeadroomBytes == 0 {
		l.ExportHeadroomBytes = v2MoveSessionBytes / 5
	}
	if l.AdmissionTimeout == 0 {
		l.AdmissionTimeout = 50 * time.Millisecond
	}
	if l.PendingPackets == 0 {
		l.PendingPackets = 32
	}
	if l.PendingPayloadBytes == 0 {
		l.PendingPayloadBytes = 38400
	}
	if l.AudioBytes == 0 {
		l.AudioBytes = 256 << 10
	}
	if l.AudioDuration == 0 {
		l.AudioDuration = 4 * time.Second
	}
	if l.ReceiptRanges == 0 {
		l.ReceiptRanges = 8
	}
	if l.ReceiptBytes == 0 {
		l.ReceiptBytes = 512
	}
	return l
}

func (l BridgeOutputLimits) Validate() error {
	l = l.Defaults()
	if l.ObserverMailboxBytes < 4096 || l.ObserverMailboxBytes > 32<<10 || l.AdapterMoveOnlyBytes < 1 || l.AdapterMoveOnlyBytes > 11<<10 || l.ObserverRequestBytes < 1 || l.ObserverRequestBytes > 32<<10 || l.ActorReserveBytes < 4096 || l.ActorReserveBytes >= v2MoveSessionBytes || l.ExportHeadroomBytes < 4096 || l.ActorReserveBytes+l.ExportHeadroomBytes >= v2MoveSessionBytes || l.AdmissionTimeout <= 0 || l.AdmissionTimeout > time.Second || l.PendingPackets < 1 || l.PendingPackets > 32 || l.PendingPayloadBytes < 1 || l.PendingPayloadBytes > 38400 || l.AudioBytes < 1 || l.AudioBytes > v2MoveSessionBytes || l.AudioDuration <= 0 || l.AudioDuration > time.Minute || l.ReceiptRanges < 2 || l.ReceiptRanges > 8 || l.ReceiptBytes < 64 || l.ReceiptBytes > 512 {
		return fmt.Errorf("%w: invalid bridge output profile", agent.ErrCapacity)
	}
	return nil
}
