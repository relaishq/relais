package mediaworker

import (
	"math"
	"time"
)

// CheckpointEnvelope bounds stale-state recovery. The worker and control plane
// must use the same configured rates. Zero values select the phase-1 envelope.
type CheckpointEnvelope struct {
	MaxAge             time.Duration
	MaxRTPPacketRate   float64 // per SSRC, packets per second
	MaxSRTCPPacketRate float64 // per SSRC, packets per second
	SafetyFactor       float64
}

func (e CheckpointEnvelope) Defaults() CheckpointEnvelope {
	if e.MaxAge <= 0 {
		e.MaxAge = 550 * time.Millisecond
	}
	if e.MaxRTPPacketRate <= 0 || math.IsNaN(e.MaxRTPPacketRate) || math.IsInf(e.MaxRTPPacketRate, 0) {
		e.MaxRTPPacketRate = 5000
	}
	if e.MaxSRTCPPacketRate <= 0 || math.IsNaN(e.MaxSRTCPPacketRate) || math.IsInf(e.MaxSRTCPPacketRate, 0) {
		e.MaxSRTCPPacketRate = 100
	}
	if e.SafetyFactor < 1 || math.IsNaN(e.SafetyFactor) || math.IsInf(e.SafetyFactor, 0) {
		e.SafetyFactor = 1.25
	}
	return e
}

// CheckpointState is persisted inside the encrypted media snapshot. Counters
// describe writes known when that snapshot committed; failures during a total
// store outage can only become durable after the next successful write.
type CheckpointState struct {
	CapturedAt          time.Time
	AgeAtCapture        time.Duration
	Attempts            uint64
	Successes           uint64
	Failures            uint64
	RTPPacketRate       float64
	SRTCPPacketRate     float64
	TakeoverAge         time.Duration
	TakeoverSnapshotAge time.Duration
	TakeoverStoredAt    time.Time
}

func (c CheckpointState) SuccessRate() float64 {
	if c.Attempts == 0 {
		return 0
	}
	return float64(c.Successes) / float64(c.Attempts)
}

// SnapshotCheckpoint exposes the persisted checkpoint and rate observations
// without exposing transport key material.
func SnapshotCheckpoint(data []byte) (CheckpointState, error) {
	snap, err := decodeSnapshot(data)
	if err != nil {
		return CheckpointState{}, err
	}
	return snap.State.Checkpoint, nil
}

// SessionCheckpoint returns local checkpoint age using elapsed local time,
// plus the age supplied by the control plane when this owner took over.
func (w *Worker) SessionCheckpoint(id string) (CheckpointState, error) {
	s := w.session(id)
	if s == nil {
		return CheckpointState{}, ErrUnknownSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.state.Checkpoint
	if !s.lastCheckpoint.IsZero() {
		info.AgeAtCapture = time.Since(s.lastCheckpoint)
	}
	return info, nil
}

// packetMeter limits encrypted packet sends and measures source index advance
// over windows of at least 100 ms. Source gaps contribute to the measured rate
// but consume one packet token, so relay loss cannot permanently block echo.
type packetMeter struct {
	at       time.Time
	tokens   float64
	highest  uint64
	anchored bool
	window   time.Time
	count    uint64
	peak     float64
}

const checkpointRTPBurst = 64
const checkpointSRTCPBurst = 16

func (m *packetMeter) allow(now time.Time, index uint64, rate float64, burst float64) bool {
	advance := uint64(1)
	if m.anchored && index > m.highest {
		advance = index - m.highest
	}
	if m.at.IsZero() {
		m.tokens = burst
		m.window = now
	} else {
		m.tokens = min(burst, m.tokens+now.Sub(m.at).Seconds()*rate)
	}
	m.at = now
	m.highest = max(m.highest, index)
	m.anchored = true
	m.count += advance
	if elapsed := now.Sub(m.window); elapsed >= 100*time.Millisecond {
		m.peak = max(m.peak, float64(m.count)/elapsed.Seconds())
		m.window, m.count = now, 0
	}
	if m.tokens < 1 {
		return false
	}
	m.tokens--
	return true
}

// CheckpointMargins scales margins beyond the base envelope, keeping the
// sequence half-space budget and SRTCP key lifetime guards in ResumeSession.
func (e CheckpointEnvelope) CheckpointMargins(age time.Duration, info CheckpointState, base uint16, rtcpBase uint32) (uint16, uint32, bool, error) {
	e = e.Defaults()
	if age < 0 || math.IsNaN(info.RTPPacketRate) || math.IsInf(info.RTPPacketRate, 0) || math.IsNaN(info.SRTCPPacketRate) || math.IsInf(info.SRTCPPacketRate, 0) {
		return 0, 0, true, ErrSequenceBudgetExhausted
	}
	rate := max(e.MaxRTPPacketRate, info.RTPPacketRate)
	rtcpRate := max(e.MaxSRTCPPacketRate, info.SRTCPPacketRate)
	required := math.Ceil(age.Seconds()*rate*e.SafetyFactor) + checkpointRTPBurst + 1
	rtcpRequired := math.Ceil(age.Seconds()*rtcpRate*e.SafetyFactor) + checkpointSRTCPBurst + 1
	outside := age > e.MaxAge || info.RTPPacketRate > e.MaxRTPPacketRate || info.SRTCPPacketRate > e.MaxSRTCPPacketRate || required > float64(base) || rtcpRequired > float64(rtcpBase)
	// The caller can keep sending during the complete two-second outage.
	// At rates above 5000/s, its half-space reserve must grow as well.
	reserve := max(float64(SequenceGapReserve), math.Ceil(2*rate)+checkpointRTPBurst)
	cap := (1 << 15) - reserve - 2
	if max(required, float64(base)) > cap || rtcpRequired > 1<<31-1 {
		return 0, 0, true, ErrSequenceBudgetExhausted
	}
	return max(base, uint16(required)), max(rtcpBase, uint32(rtcpRequired)), outside, nil
}

// CallerSequenceReserve is the bounded source advance over the phase-1
// two-second recovery target. Call after CheckpointMargins accepts the state.
func (e CheckpointEnvelope) CallerSequenceReserve(info CheckpointState, outage time.Duration) uint32 {
	e = e.Defaults()
	reserve := max(float64(SequenceGapReserve), math.Ceil(max(outage, 2*time.Second).Seconds()*max(e.MaxRTPPacketRate, info.RTPPacketRate))+checkpointRTPBurst)
	return uint32(min(reserve, 1<<15))
}
