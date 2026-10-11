package mediaworker

import (
	"math"
	"time"
)

// CheckpointEnvelope bounds stale-state recovery. The worker and control plane
// must use the same configured rates. Zero values select the phase-1 envelope.
type CheckpointEnvelope struct {
	MaxAge              time.Duration
	MaxRTPPacketRate    float64 // per SSRC, packets per second
	MaxSRTCPPacketRate  float64 // per SSRC, packets per second
	SafetyFactor        float64
	RTPBacklogAllowance uint32 // zero selects 64 indexes of jitter/backlog
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
	if e.RTPBacklogAllowance == 0 {
		e.RTPBacklogAllowance = checkpointRTPBurst
	}
	return e
}

// CheckpointState is persisted inside the encrypted media snapshot. Counters
// describe writes known when that snapshot committed; failures during a total
// store outage can only become durable after the next successful write.
type CheckpointState struct {
	CapturedAt          time.Time     `json:"captured_at"`
	AgeAtCapture        time.Duration `json:"age_at_capture"`
	Attempts            uint64        `json:"attempts"`
	Successes           uint64        `json:"successes"`
	Failures            uint64        `json:"failures"`
	RTPPacketRate       float64       `json:"rtp_packet_rate"`
	SRTCPPacketRate     float64       `json:"srtcp_packet_rate"`
	TakeoverAge         time.Duration `json:"takeover_age"`
	TakeoverSnapshotAge time.Duration `json:"takeover_snapshot_age"`
	TakeoverStoredAt    time.Time     `json:"takeover_stored_at"`
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

// sourceRate measures source sequence advance in RTP media time, not arrival
// time. A hold-release burst preserves timestamps and cannot inflate this rate.
// Measurements cover at least two media seconds; peaks decay with a ten-second
// half-life, including after a move. The configured contract remains the floor.
type sourceRate struct {
	anchored      bool
	index         uint64
	timestamp     uint32
	lastIndex     uint64
	lastTimestamp uint32
	peak          float64
	peakAt        time.Time
}

const checkpointRTPBurst = 64
const checkpointSRTCPBurst = 16

func (m *sourceRate) rate(now time.Time) float64 {
	if m.peakAt.IsZero() {
		return 0
	}
	return m.peak * math.Exp2(-max(now.Sub(m.peakAt).Seconds(), 0)/10)
}

func (m *sourceRate) observe(now time.Time, index uint64, timestamp, clockRate uint32) {
	if !m.anchored {
		m.anchored, m.index, m.timestamp = true, index, timestamp
		m.lastIndex, m.lastTimestamp = index, timestamp
		return
	}
	// Ignore reordered sequence indexes before interpreting timestamp changes.
	if index <= m.lastIndex {
		return
	}
	// Signed deltas preserve rollover. A backward timestamp on newer media
	// starts a new measurement window, retaining the decaying prior peak.
	backward := int32(timestamp-m.lastTimestamp) < 0
	m.lastIndex, m.lastTimestamp = index, timestamp
	if backward {
		m.index, m.timestamp = index, timestamp
		return
	}
	ticks := uint32(timestamp - m.timestamp)
	if ticks < 2*clockRate {
		return
	}
	measured := float64(index-m.index) * float64(clockRate) / float64(ticks)
	m.peak, m.peakAt = max(measured, m.rate(now)), now
	m.index, m.timestamp = index, timestamp
}

// checkpointRates samples under the session packet lock. Live exports must
// decay observations too, even when the background checkpoint writer stalled.
func (s *session) checkpointRates(info *CheckpointState) {
	now := time.Now()
	info.RTPPacketRate = max(s.audioRate.rate(now), s.videoRate.rate(now))
	info.SRTCPPacketRate = s.rtcpRate.rate(now)
}

// packetMeter retains the SRTCP send bucket. These indexes are allocated by
// the worker, so limiting encryptions also limits their advance.
type packetMeter struct {
	at     time.Time
	tokens float64
	window time.Time
	count  uint64
	peak   float64
	peakAt time.Time
}

func (m *packetMeter) rate(now time.Time) float64 {
	if m.peakAt.IsZero() {
		return 0
	}
	return m.peak * math.Exp2(-max(now.Sub(m.peakAt).Seconds(), 0)/10)
}

func (m *packetMeter) allow(now time.Time, _ uint64, rate float64, burst float64) bool {
	if m.at.IsZero() {
		m.tokens, m.window = burst, now
	} else {
		m.tokens = min(burst, m.tokens+now.Sub(m.at).Seconds()*rate)
	}
	m.at = now
	if m.tokens < 1 {
		return false
	}
	m.tokens--
	m.count++
	if elapsed := now.Sub(m.window); elapsed >= 2*time.Second {
		m.peak, m.peakAt = max(float64(m.count)/elapsed.Seconds(), m.rate(now)), now
		m.window, m.count = now, 0
	}
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
	required := math.Ceil(age.Seconds()*rate*e.SafetyFactor) + float64(e.RTPBacklogAllowance) + 1
	rtcpRequired := math.Ceil(age.Seconds()*rtcpRate*e.SafetyFactor) + checkpointSRTCPBurst + 1
	outside := age > e.MaxAge || info.RTPPacketRate > e.MaxRTPPacketRate || info.SRTCPPacketRate > e.MaxSRTCPPacketRate || required > float64(base) || rtcpRequired > float64(rtcpBase)
	// The caller can keep sending during the complete two-second outage.
	// At rates above 5000/s, its half-space reserve must grow as well.
	reserve := max(float64(SequenceGapReserve), math.Ceil(2*rate)+float64(e.RTPBacklogAllowance))
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
	reserve := max(float64(SequenceGapReserve), math.Ceil(max(outage, 2*time.Second).Seconds()*max(e.MaxRTPPacketRate, info.RTPPacketRate))+float64(e.RTPBacklogAllowance))
	return uint32(min(reserve, 1<<15))
}
