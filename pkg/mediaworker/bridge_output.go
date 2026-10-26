package mediaworker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

const bridgeOutputVersion = 1
const bridgeOutputMoveVersion = 2
const bridgeBinaryBytes = 1200
const bridgeRecordCharge = 512
const bridgeMetadataPartition = v2HostMetadataBytes / 2
const bridgeTerminalReserve = 3 * bridgeMetadataPartition / 8

var (
	ErrBridgeOutputUnbound   = errors.New("bridge: output scope is not bound")
	ErrBridgeOutputScope     = errors.New("bridge: stale output scope")
	ErrBridgeOutputMoving    = errors.New("bridge: output moving")
	ErrBridgeOutputUncertain = errors.New("bridge: recovered output ceiling requires reconciliation")
	ErrBridgeOutputOrder     = errors.New("bridge: output sequence must be next")
)

// BridgeOutputScope is copied from an authenticated attachment, never from
// untrusted packet fields. Binding/reauthorization belongs to the bridge owner.
type BridgeOutputScope struct {
	SessionID   string `json:"session_id"`
	BotInstance string `json:"bot_instance"`
	Epoch       uint64 `json:"epoch"`
	Generation  uint64 `json:"generation"`
	LineageID   string `json:"lineage_id"`
}

func bridgeToken(v string) bool {
	if len(v) == 0 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
func (v BridgeOutputScope) valid(id string) bool {
	return v.SessionID == id && bridgeToken(v.SessionID) && bridgeToken(v.BotInstance) && v.Epoch > 0 && v.Generation > 0 && bridgeToken(v.LineageID)
}

type BridgeOutputReceipt struct {
	BridgeOutputScope
	Sequence uint64     `json:"seq"`
	Turn     agent.Turn `json:"turn"`
	Revision uint64     `json:"revision"`
	Outcome  string     `json:"outcome,omitempty"`
	Status   string     `json:"status,omitempty"`
	Consumed bool       `json:"consumed"`
	Reason   string     `json:"reason,omitempty"`
}
type BridgeOutputCredit struct {
	BridgeOutputScope
	Revision        uint64 `json:"revision"`
	ThroughSequence uint64 `json:"through_seq"`
	PacketSlots     int    `json:"packet_slots"`
	PayloadBytes    int    `json:"payload_bytes"`
}
type BridgeOutputStatus struct {
	BridgeOutputScope
	Sequence uint64               `json:"seq"`
	Revision uint64               `json:"revision"`
	Status   string               `json:"status"`
	Receipt  *BridgeOutputReceipt `json:"receipt,omitempty"`
}
type BridgeInterruption struct {
	BridgeOutputScope
	Canceled    []uint64   `json:"-"` // owned identities canceled at this exact commit
	RequestID   string     `json:"request_id"`
	CancelFloor agent.Turn `json:"cancel_floor"`
	ClearFloor  agent.Turn `json:"clear_floor"`
	Revision    uint64     `json:"revision"`
}
type BridgePlaybackFinished struct {
	Turn     agent.Turn `json:"turn"`
	Outcome  string     `json:"outcome"`
	GapCount uint64     `json:"gap_count"`
}

// Compact crash metadata. Payloads and digests are private exact-move data.
// A settled floor includes admitted identities, not a claim of durable send.
type bridgeReceiptRange struct {
	First   uint64     `json:"f"`
	Last    uint64     `json:"l"`
	Turn    agent.Turn `json:"t"`
	Outcome string     `json:"o"`
	Reason  string     `json:"r,omitempty"`
}
type bridgeTurn struct {
	First    uint64     `json:"b,omitempty"`
	Turn     agent.Turn `json:"t"`
	Stream   uint64     `json:"s"`
	Last     uint64     `json:"l,omitempty"`
	Ended    bool       `json:"e,omitempty"`
	Outcome  string     `json:"o,omitempty"`
	Gaps     uint64     `json:"g,omitempty"`
	Finished bool       `json:"f,omitempty"`
}
type bridgeOutputIdentity struct {
	SessionID   string `json:"s"`
	BotInstance string `json:"b"`
	Epoch       uint64 `json:"e"`
	Generation  uint64 `json:"g"`
	LineageID   string `json:"l"`
}

func (v bridgeOutputIdentity) valid(id string) bool { return BridgeOutputScope(v).valid(id) }

type bridgeOutputState struct {
	Version     int                  `json:"v"`
	Scope       bridgeOutputIdentity `json:"scope"`
	Revision    uint64               `json:"r"`
	Through     uint64               `json:"f,omitempty"`
	Sent        uint64               `json:"s,omitempty"`
	Pruned      uint64               `json:"p,omitempty"`
	Exceptions  []bridgeReceiptRange `json:"x,omitempty"`
	CancelFloor agent.Turn           `json:"c,omitempty"`
	ClearFloor  agent.Turn           `json:"a,omitempty"`
	Turns       []bridgeTurn         `json:"t,omitempty"`
	LastTurn    agent.Turn           `json:"u,omitempty"`
	LastStream  uint64               `json:"m,omitempty"`
	// RebaseTicks records unused timestamp advancement from late wakes. Gaps
	// consume this first, without changing nextAt or charging RTP time twice.
	RebaseTicks   uint64 `json:"b,omitempty"`
	RebaseThrough uint64 `json:"h,omitempty"`
}

func cloneBridgeState(v *bridgeOutputState) *bridgeOutputState {
	if v == nil {
		return nil
	}
	out := *v
	out.Exceptions = slices.Clone(v.Exceptions)
	out.Turns = slices.Clone(v.Turns)
	return &out
}

type bridgePacketRecord struct {
	Scope    BridgeOutputScope   `json:"scope"`
	Receipt  BridgeOutputReceipt `json:"receipt"`
	Digest   [sha256.Size]byte   `json:"digest"`
	Duration time.Duration       `json:"duration"`
	Length   int                 `json:"length"`
	Stream   uint64              `json:"stream"`
	Payload  []byte              `json:"payload,omitempty"`
}
type bridgeQueued struct {
	Sequence uint64        `json:"packet_seq"`
	Last     uint64        `json:"last,omitempty"`
	Turn     agent.Turn    `json:"turn"`
	Stream   uint64        `json:"stream_id"`
	Duration time.Duration `json:"duration"`
	Kind     string        `json:"kind"`
	Offset   uint32        `json:"offset"`
	Count    uint32        `json:"count"`
}

func (q bridgeQueued) isGap() bool { return q.Kind == "gap" }

type bridgeOutputMove struct {
	CreditRevision     uint64               `json:"credit_revision,omitempty"`
	Uncertain          bool                 `json:"uncertain,omitempty"`
	Version            int                  `json:"version"`
	State              *bridgeOutputState   `json:"state"`
	Records            []bridgePacketRecord `json:"records,omitempty"`
	Settled            []bridgeSettledRange `json:"settled,omitempty"`
	Queue              []bridgeQueued       `json:"queue,omitempty"`
	Durable            *bridgeOutputState   `json:"durable,omitempty"`
	CheckpointSequence uint64               `json:"checkpoint_seq,omitempty"`
	ReplayHorizon      uint64               `json:"replay_horizon,omitempty"`
}
type bridgeOutputRequest struct {
	invalid  bool
	scope    BridgeOutputScope
	seq      uint64
	turn     agent.Turn
	data     []byte
	digest   [sha256.Size]byte
	duration time.Duration
	ingress  time.Time
	done     chan BridgeOutputReceipt
}
type bridgePriorityRequest struct {
	scope     BridgeOutputScope
	requestID string
	floor     agent.Turn
	clear     bool
	cancel    bool
	done      chan bridgePriorityReply
}
type bridgePriorityReply struct {
	receipt BridgeInterruption
	err     error
}

type bridgeAttempt struct {
	turn   agent.Turn
	cancel context.CancelFunc
}
type bridgeOutputHost struct {
	draining         int // guarded by session.mu; pending moves stop credit before pause
	changed          chan struct{}
	provenUnadmitted bool
	state            *bridgeOutputState // immutable accepted revision; guarded by session.mu
	settled          []bridgeSettledRange
	settledCost      atomic.Int64
	settledLive      atomic.Int64
	records          map[uint64]bridgePacketRecord
	ingress          chan bridgeOutputRequest
	priority         chan bridgePriorityRequest
	reservedPackets  atomic.Int64
	reservedBytes    atomic.Int64
	memory           atomic.Int64
	encodingBytes    atomic.Int64
	recordCount      atomic.Int64
	recordMemory     atomic.Int64
	audioBytes       int
	audioDuration    time.Duration
	replayHorizon    uint64
	durable          *bridgeOutputState
	creditRevision   atomic.Uint64
	checkpointSeq    uint64
	attempts         map[agent.TaskID]bridgeAttempt
	limits           BridgeOutputLimits
	// The revision is exported under the existing paused/fenced session lock.
	// No transport callback is involved in capture.
}

// BindBridgeOutput is an internal-owner seam for the future authenticated bridge
// transport. It cannot turn an ordinary agent into a bridge or replace lineage.
// The caller must establish authorization before binding the first scope.
func (w *Worker) BindBridgeOutput(scope BridgeOutputScope) error {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.agent.v2.bridge
	if !scope.valid(s.id) || (s.lease.Epoch > 0 && scope.Epoch != s.lease.Epoch) {
		return ErrBridgeOutputScope
	}
	if b.state != nil {
		if b.state.Scope != bridgeOutputIdentity(scope) {
			return ErrBridgeOutputScope
		}
		return nil
	}
	b.state = &bridgeOutputState{Version: bridgeOutputVersion, Scope: bridgeOutputIdentity(scope), Revision: 1}
	s.projectBridgeLocked(s.state.Agent.V2)
	return nil
}
func (w *Worker) bridgeSession(id string) (*session, error) {
	w.mu.Lock()
	s := w.sessions[id]
	w.mu.Unlock()
	if s == nil {
		return nil, ErrBridgeOutputScope
	}
	if s.agent == nil || s.agent.v2 == nil || s.agent.v2.bridge == nil {
		return nil, ErrBridgeOutputScope
	}
	return s, nil
}

// BeginBridgeOutput reserves the host turn before observer task startup. The
// bounded marker, not the bridge.audio/v1 lifetime handler, controls completion.
func (w *Worker) BeginBridgeOutput(scope BridgeOutputScope, turn agent.Turn) (uint64, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return 0, err
	}
	return s.beginBridgeOutput(scope, turn)
}
func (s *session) beginBridgeOutput(scope BridgeOutputScope, turn agent.Turn) (uint64, error) {
	deadline := time.Now().Add(s.agent.v2.bridge.limits.AdmissionTimeout)
	for {
		s.mu.Lock()
		b := s.agent.v2.bridge
		if err := s.bridgeScopeLocked(scope); err != nil {
			s.mu.Unlock()
			return 0, err
		}
		if turn < max(b.state.CancelFloor, b.state.ClearFloor) {
			s.mu.Unlock()
			return 0, ErrBridgeOutputScope
		}
		for _, t := range b.state.Turns {
			if t.Turn == turn {
				s.mu.Unlock()
				return t.Stream, nil
			}
		}
		if len(b.state.Turns) >= b.limits.PendingPackets || len(b.state.Exceptions) >= b.limits.ReceiptRanges-1 {
			s.mu.Unlock()
			return 0, agent.ErrBackpressure
		}
		v := cloneBridgeState(b.state)
		if v.LastStream > 0 && turn <= v.LastTurn {
			s.mu.Unlock()
			return 0, ErrBridgeOutputOrder
		}
		stream := v.LastStream + 1
		if stream == 0 {
			s.mu.Unlock()
			return 0, agent.ErrCapacity
		}
		v.LastTurn = turn
		v.LastStream = stream
		v.Revision++
		v.Turns = append(v.Turns, bridgeTurn{Turn: turn, Stream: stream})
		base := b.state
		view := s.playbackSnapshotLocked()
		s.mu.Unlock()
		charge, err := bridgeInterruptionCharge(v, view)
		if err != nil || charge+bridgeTerminalReserve > bridgeMetadataPartition {
			return 0, agent.ErrBackpressure
		}
		s.mu.Lock()
		if b.state != base {
			s.mu.Unlock()
			if time.Now().After(deadline) {
				return 0, agent.ErrBackpressure
			}
			continue
		}
		if err = s.bridgeScopeLocked(scope); err != nil {
			s.mu.Unlock()
			return 0, err
		}
		b.state = v
		s.projectBridgeLocked(s.state.Agent.V2)
		s.mu.Unlock()
		return stream, nil
	}
}
func (s *session) bridgeScopeLocked(scope BridgeOutputScope) error {
	b := s.agent.v2.bridge
	if b.state == nil {
		return ErrBridgeOutputUnbound
	}
	if b.state.Scope != bridgeOutputIdentity(scope) {
		current := BridgeOutputScope(b.state.Scope)
		if current.SessionID == scope.SessionID && current.BotInstance == scope.BotInstance && current.Generation == scope.Generation && current.LineageID == scope.LineageID && current.Epoch != scope.Epoch {
			return ErrBridgeOutputUnbound
		}
		return ErrBridgeOutputScope
	}
	if s.lease.Epoch > 0 && b.state.Scope.Epoch != s.lease.Epoch {
		return ErrBridgeOutputUncertain
	}
	if s.fenced.Load() || s.agent.paused {
		return ErrBridgeOutputMoving
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	return nil
}

// SubmitBridgeOutput takes the ingress timestamp before any admission wait.
// A canceled caller has an uncertain reply: Status must precede a retry.
func (w *Worker) SubmitBridgeOutput(ctx context.Context, scope BridgeOutputScope, turn agent.Turn, seq uint64, opus []byte) (BridgeOutputReceipt, error) {
	ingress := time.Now()
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return BridgeOutputReceipt{}, err
	}
	return s.submitBridgeOutput(ctx, scope, turn, seq, opus, ingress)
}
func (s *session) submitBridgeOutput(ctx context.Context, scope BridgeOutputScope, turn agent.Turn, seq uint64, opus []byte, ingress time.Time) (BridgeOutputReceipt, error) {
	b := s.agent.v2.bridge
	s.mu.Lock()
	uncertain := !b.provenUnadmitted
	s.mu.Unlock()
	if uncertain {
		return BridgeOutputReceipt{}, ErrBridgeOutputUncertain
	}
	if len(opus) > bridgeBinaryBytes {
		return BridgeOutputReceipt{}, agent.ErrIncompatibleAgent
	}
	if seq == 0 || seq == ^uint64(0) {
		return BridgeOutputReceipt{}, ErrBridgeOutputOrder
	}
	// Reserve before copying, including the item being prepared and the one the
	// loop is committing. Full ingress never grows an unbounded socket queue.
	if b.reservedPackets.Add(1) > int64(b.limits.PendingPackets) {
		b.reservedPackets.Add(-1)
		return BridgeOutputReceipt{}, agent.ErrBackpressure
	}
	if b.reservedBytes.Add(int64(len(opus))) > int64(b.limits.PendingPayloadBytes) {
		b.reservedBytes.Add(-int64(len(opus)))
		b.reservedPackets.Add(-1)
		return BridgeOutputReceipt{}, agent.ErrBackpressure
	}
	if err := s.reserveBridgeMemory(int64(2*len(opus) + 64)); err != nil {
		b.reservedBytes.Add(-int64(len(opus)))
		b.reservedPackets.Add(-1)
		return BridgeOutputReceipt{}, err
	}
	owned := slices.Clone(opus)
	duration, validationErr := opusDuration(owned)
	request := bridgeOutputRequest{invalid: validationErr != nil, scope: scope, seq: seq, turn: turn, data: owned, digest: sha256.Sum256(owned), duration: duration, ingress: ingress, done: make(chan BridgeOutputReceipt, 1)}
	select {
	case b.ingress <- request:
	case <-ctx.Done():
		s.releaseBridgeIngress(request)
		return BridgeOutputReceipt{}, ctx.Err()
	case <-s.ctx.Done():
		s.releaseBridgeIngress(request)
		return BridgeOutputReceipt{}, s.ctx.Err()
	}
	select {
	case receipt := <-request.done:
		return receipt, nil
	case <-ctx.Done():
		return BridgeOutputReceipt{}, ctx.Err()
	case <-s.ctx.Done():
		return BridgeOutputReceipt{}, s.ctx.Err()
	}
}
func (s *session) releaseBridgeIngress(r bridgeOutputRequest) {
	b := s.agent.v2.bridge
	_ = s.reserveBridgeMemory(-int64(2*len(r.data) + 64))
	b.reservedBytes.Add(-int64(len(r.data)))
	b.reservedPackets.Add(-1)
	b.signalChange()
}
func (s *session) bridgeOutputLoop() {
	b := s.agent.v2.bridge
	for {
		// Priority is served before each packet, independent of packet credit.
		select {
		case r := <-b.priority:
			s.applyBridgePriority(r)
			continue
		default:
		}
		select {
		case <-s.ctx.Done():
			return
		case r := <-b.priority:
			s.applyBridgePriority(r)
		case r := <-b.ingress:
			receipt := s.admitBridgePacket(r)
			s.releaseBridgeIngress(r)
			r.done <- receipt
		}
	}
}
func (s *session) admitBridgePacket(r bridgeOutputRequest) BridgeOutputReceipt {
	for {
		receipt, retry := s.admitBridgeAttempt(r, false)
		if !retry {
			return receipt
		}
		if time.Since(r.ingress) >= s.agent.v2.bridge.limits.AdmissionTimeout {
			receipt, _ = s.admitBridgeAttempt(r, true)
			return receipt
		}
	}
}
func (s *session) admitBridgeAttempt(r bridgeOutputRequest, terminal bool) (BridgeOutputReceipt, bool) {
	b := s.agent.v2.bridge
	receipt := BridgeOutputReceipt{BridgeOutputScope: r.scope, Sequence: r.seq, Turn: r.turn, Outcome: "refused"}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bridgeScopeLocked(r.scope); err != nil {
		switch {
		case errors.Is(err, ErrBridgeOutputMoving):
			receipt.Reason = "moving"
		case errors.Is(err, ErrBridgeOutputUnbound), errors.Is(err, ErrBridgeOutputUncertain):
			receipt.Reason = "unbound"
		case errors.Is(err, context.Canceled):
			receipt.Reason = "closed"
		default:
			receipt.Reason = "stale_scope"
		}
		return receipt, false
	}
	receipt.Revision = b.state.Revision
	if prior, ok := b.records[r.seq]; ok {
		if prior.Scope != r.scope {
			receipt.Reason = "stale_scope"
			return receipt, false
		}
		if prior.Digest != r.digest || prior.Receipt.Turn != r.turn {
			receipt.Consumed = true
			receipt.Reason = "conflict"
			return receipt, false
		}
		return prior.Receipt, false
	}
	if prior, digest, ok := b.settledReceipt(r.seq); ok {
		if digest != r.digest || prior.Turn != r.turn {
			receipt.Consumed, receipt.Reason = true, "conflict"
			return receipt, false
		}
		return prior, false
	}
	if r.seq <= b.state.Pruned {
		receipt.Outcome = ""
		receipt.Status = "settled"
		receipt.Consumed = true
		receipt.Reason = "settled"
		return receipt, false
	}
	if !b.provenUnadmitted {
		receipt.Outcome = "unknown"
		receipt.Reason = "uncertain"
		return receipt, false
	}
	if r.seq != b.state.Through+1 {
		receipt.Reason = "out_of_order"
		return receipt, false
	}
	i := slices.IndexFunc(b.state.Turns, func(t bridgeTurn) bool { return t.Turn == r.turn })
	closed := i < 0 || (b.state.Turns[i].Ended && r.turn >= max(b.state.CancelFloor, b.state.ClearFloor))
	v := cloneBridgeState(b.state)
	if !closed && v.Turns[i].First == 0 {
		v.Turns[i].First = r.seq
	}
	receipt.Outcome, receipt.Reason = "admitted", ""
	switch {
	case closed:
		receipt.Outcome, receipt.Reason = "refused", "closed_turn"
	case r.turn < max(v.CancelFloor, v.ClearFloor):
		receipt.Outcome = "canceled"
	case r.invalid:
		receipt.Outcome = "refused"
		receipt.Reason = "invalid_audio"
	case terminal || time.Since(r.ingress) >= b.limits.AdmissionTimeout:
		receipt.Outcome = "refused"
		receipt.Reason = "output_late"
	case b.audioBytes+len(r.data) > min(b.limits.AudioBytes, s.agent.v2.registration.Bounds.MaxAudioBytes) || b.audioDuration+r.duration > b.limits.AudioDuration:
		receipt.Outcome = "refused"
		receipt.Reason = "backpressure"
	}
	if receipt.Outcome != "admitted" {
		v.Exceptions = addBridgeRange(v.Exceptions, bridgeReceiptRange{First: r.seq, Last: r.seq, Turn: r.turn, Outcome: receipt.Outcome, Reason: receipt.Reason})
		// Check both the range count and its actual JSON representation length.
		// Pressure leaves this identity unadmitted, without a success receipt.
		if len(v.Exceptions) > b.limits.ReceiptRanges || bridgeRangesBytes(v.Exceptions) > b.limits.ReceiptBytes {
			receipt.Reason = "backpressure"
			receipt.Consumed = false
			receipt.Revision = b.state.Revision
			return receipt, false
		}
	}
	v.Through = r.seq
	v.Revision++
	receipt.Revision = v.Revision
	receipt.Consumed = true
	var stream uint64
	if !closed {
		stream = v.Turns[i].Stream
	}
	// A terminal refusal for a closed turn consumes the identity without
	// reopening its completed playback or introducing new media time.
	duration := r.duration
	if closed {
		duration = 0
	}
	record := bridgePacketRecord{Scope: r.scope, Receipt: receipt, Digest: r.digest, Duration: duration, Length: len(r.data), Stream: stream}
	queued := bridgeQueued{Kind: "bridge", Count: 1, Sequence: r.seq, Last: r.seq, Turn: r.turn, Stream: record.Stream, Duration: duration}
	if receipt.Outcome == "admitted" {
		record.Payload = r.data
	} else if !closed {
		queued.Kind = "gap"
		if receipt.Outcome != "canceled" {
			v.Turns[i].Gaps++
		}
		if receipt.Outcome == "canceled" {
			v.Turns[i].Outcome = "canceled"
		} else {
			v.Turns[i].Outcome = "gapped"
		}
	}
	if !terminal {
		base, actorBase := b.state, s.state.Agent.V2
		projected := s.agent.v2.playback
		projected.bridgeQueue = slices.Clone(projected.bridgeQueue)
		if receipt.Outcome != "canceled" && duration > 0 {
			projected.bridgeQueue = appendBridgeQueued(projected.bridgeQueue, queued, v.RebaseThrough)
		}
		projected.length += len(record.Payload)
		if record.Payload != nil {
			projected.duration += r.duration
		}
		view := projected.bridgeSnapshotWith(&record)
		s.mu.Unlock()
		charge, err := bridgeInterruptionCharge(v, view)
		s.mu.Lock()
		if b.state != base || s.state.Agent.V2 != actorBase {
			return receipt, true
		}
		if err != nil || charge > bridgeMetadataPartition {
			receipt.Outcome = "refused"
			receipt.Reason = "backpressure"
			receipt.Consumed = false
			receipt.Revision = b.state.Revision
			return receipt, false
		}
		if receipt.Outcome == "admitted" && time.Since(r.ingress) >= b.limits.AdmissionTimeout {
			return receipt, true
		}
	}
	// Receipt digests, private references, and payload all share the mailbox.
	if receipt.Outcome == "admitted" && s.bridgeExportCostLocked()+bridgePacketExportCost(record) > s.bridgeLookaheadBudget() {
		receipt.Outcome, receipt.Reason, receipt.Consumed, receipt.Revision = "refused", "backpressure", false, b.state.Revision
		return receipt, false
	}
	charge := int64(bridgeRecordCharge + len(record.Payload))
	if err := s.reserveBridgeMemory(charge); err != nil {
		receipt.Outcome = "refused"
		receipt.Reason = "backpressure"
		receipt.Consumed = false
		receipt.Revision = b.state.Revision
		return receipt, false
	}
	b.replaceRecordEncoding(nil, &record)
	b.records[r.seq] = record
	b.state = v
	p := &s.agent.v2.playback
	if receipt.Outcome != "canceled" && duration > 0 {
		p.bridgeQueue = appendBridgeQueued(p.bridgeQueue, queued, v.RebaseThrough)
	} else {
		advanceBridgeSent(b, v)
	}
	if record.Payload != nil {
		b.audioBytes += len(record.Payload)
		b.audioDuration += r.duration
		p.length += len(record.Payload)
		p.duration += r.duration
	}
	s.projectBridgeLocked(s.state.Agent.V2)
	s.wakePacer()
	return receipt, false
}
func bridgeRangesBytes(ranges []bridgeReceiptRange) int {
	if len(ranges) == 0 {
		return 2
	}
	n := 2 + len(ranges) - 1
	for _, r := range ranges {
		n += len(`{"f":,"l":,"t":,"o":""}`) + len(strconv.FormatUint(r.First, 10)) + len(strconv.FormatUint(r.Last, 10)) + len(strconv.FormatUint(uint64(r.Turn), 10)) + len(r.Outcome)
		if r.Reason != "" {
			n += len(`,"r":""`) + len(r.Reason)
		}
	}
	return n
}
func addBridgeRange(ranges []bridgeReceiptRange, r bridgeReceiptRange) []bridgeReceiptRange {
	out := slices.Clone(ranges)
	if len(out) > 0 {
		last := &out[len(out)-1]
		if last.Last+1 == r.First && last.Turn == r.Turn && last.Outcome == r.Outcome && last.Reason == r.Reason {
			last.Last = r.Last
			return out
		}
	}
	return append(out, r)
}
func (s *session) reserveBridgeMemory(delta int64) error {
	b := s.agent.v2.bridge
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.moveClosed {
		return context.Canceled
	}
	n := b.memory.Load() + delta
	if n < 0 || n > s.bridgeMemoryLimitLocked() {
		return agent.ErrBackpressure
	}
	if delta > 0 && !s.worker.reserveMoveBytesWithin(delta, s.snapshotLimits().MoveBytesTotal-int64(v2ExportScratchBytes)) {
		return ErrMoveBudget
	}
	if delta < 0 {
		s.worker.moveBytes.Add(delta)
	}
	b.memory.Store(n)
	return nil
}
func (s *session) projectBridgeLocked(e *v2Envelope) {
	if s.agent.v2.bridge == nil || s.agent.v2.bridge.state == nil {
		return
	}
	b := s.agent.v2.bridge
	e.Bridge = cloneBridgeState(b.state)
	e.CancelFloor = b.state.CancelFloor
	e.Playback = s.playbackSnapshotLocked()
}

func (w *Worker) BridgeOutputStatus(scope BridgeOutputScope, seq uint64) (BridgeOutputStatus, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return BridgeOutputStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.bridgeScopeLocked(scope); err != nil {
		return BridgeOutputStatus{}, err
	}
	b := s.agent.v2.bridge
	status := BridgeOutputStatus{BridgeOutputScope: scope, Sequence: seq, Revision: b.state.Revision, Status: "unknown"}
	if r, ok := b.records[seq]; ok {
		if r.Scope != scope {
			return status, nil
		}
		receipt := r.Receipt
		status.Status = "settled"
		status.Receipt = &receipt
	} else if receipt, _, ok := b.settledReceipt(seq); ok {
		status.Status, status.Receipt = "settled", &receipt
	} else if bridgeSettledContains(b.settled, seq) {
		return status, nil // An old scope cannot lend its sent floor to this identity.
	} else if seq <= b.state.Pruned {
		status.Status = "settled"
	} else if seq <= b.state.Sent {
		status.Status = "settled"
		receipt := BridgeOutputReceipt{BridgeOutputScope: scope, Sequence: seq, Revision: b.state.Revision, Outcome: "sent", Consumed: true}
		for _, r := range b.state.Exceptions {
			if seq >= r.First && seq <= r.Last {
				receipt.Turn = r.Turn
				receipt.Outcome = r.Outcome
				receipt.Reason = r.Reason
				break
			}
		}
		status.Receipt = &receipt
	} else if seq > b.state.Through && b.provenUnadmitted {
		status.Status = "unadmitted"
	}
	return status, nil
}
func (w *Worker) BridgeOutputCredit(scope BridgeOutputScope) (BridgeOutputCredit, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return BridgeOutputCredit{}, err
	}
	s.mu.Lock()
	b := s.agent.v2.bridge
	if err = s.bridgeScopeLocked(scope); err != nil {
		s.mu.Unlock()
		return BridgeOutputCredit{}, err
	}
	defer s.mu.Unlock()
	// Charge the same cut that supplies credit. A pacer wake or checkpoint may
	// otherwise replace the immutable state while encoding, without using any
	// capacity; that race must not withdraw available producer credit.
	metadata, encodeErr := bridgeCreditMetadataCharge(b.state, s.playbackSnapshotLocked(), b.limits.PendingPackets)
	slots := b.limits.PendingPackets - int(b.reservedPackets.Load())
	bytes := min(b.limits.PendingPayloadBytes-int(b.reservedBytes.Load()), b.limits.AudioBytes-b.audioBytes-int(b.reservedBytes.Load()))
	if !b.provenUnadmitted || encodeErr != nil || metadata+bridgeTerminalReserve > bridgeMetadataPartition || b.audioBytes >= b.limits.AudioBytes || b.audioDuration >= b.limits.AudioDuration || s.bridgeCreditExhaustedLocked() || len(b.state.Exceptions) >= b.limits.ReceiptRanges-1 {
		slots = 0
		bytes = 0
	}
	slots, bytes = s.bridgeLookaheadCreditLocked(slots, bytes)
	return BridgeOutputCredit{BridgeOutputScope: scope, Revision: b.creditRevision.Add(1), ThroughSequence: b.state.Through, PacketSlots: max(0, slots), PayloadBytes: max(0, bytes)}, nil
}
func (w *Worker) InterruptBridgeOutput(ctx context.Context, scope BridgeOutputScope, requestID string, floor agent.Turn, clear bool) (BridgeInterruption, error) {
	return w.bridgeInterrupt(ctx, scope, requestID, floor, true, clear)
}
func (w *Worker) ClearBridgeAudio(ctx context.Context, scope BridgeOutputScope, requestID string, before agent.Turn) (BridgeInterruption, error) {
	return w.bridgeInterrupt(ctx, scope, requestID, before, false, true)
}
func (w *Worker) bridgeInterrupt(ctx context.Context, scope BridgeOutputScope, requestID string, floor agent.Turn, cancel, clear bool) (BridgeInterruption, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return BridgeInterruption{}, err
	}
	if requestID == "" || len(requestID) > 128 {
		return BridgeInterruption{}, agent.ErrCapacity
	}
	r := bridgePriorityRequest{scope: scope, requestID: requestID, floor: floor, clear: clear, cancel: cancel, done: make(chan bridgePriorityReply, 1)}
	select {
	case s.agent.v2.bridge.priority <- r:
	case <-ctx.Done():
		return BridgeInterruption{}, ctx.Err()
	case <-s.ctx.Done():
		return BridgeInterruption{}, s.ctx.Err()
	}
	select {
	case out := <-r.done:
		return out.receipt, out.err
	case <-ctx.Done():
		return BridgeInterruption{}, ctx.Err()
	case <-s.ctx.Done():
		return BridgeInterruption{}, s.ctx.Err()
	}
}
func (s *session) applyBridgePriority(r bridgePriorityRequest) {
	s.mu.Lock()
	err := s.bridgeScopeLocked(r.scope)
	receipt := BridgeInterruption{BridgeOutputScope: r.scope, RequestID: r.requestID}
	if err == nil {
		receipt.Canceled = s.interruptBridgeLocked(r.floor, r.cancel, r.clear)
		b := s.agent.v2.bridge
		receipt.CancelFloor, receipt.ClearFloor, receipt.Revision = b.state.CancelFloor, b.state.ClearFloor, b.state.Revision
	}
	s.mu.Unlock()
	r.done <- bridgePriorityReply{receipt, err}
}
func (s *session) interruptBridgeLocked(floor agent.Turn, cancel, clear bool) []uint64 {
	var canceled []uint64
	b := s.agent.v2.bridge
	v := cloneBridgeState(b.state)
	if cancel {
		v.CancelFloor = max(v.CancelFloor, floor)
	}
	if clear {
		v.ClearFloor = max(v.ClearFloor, floor)
	}
	v.Revision++
	for i := range v.Turns {
		if !v.Turns[i].Finished && v.Turns[i].Turn < floor {
			v.Turns[i].Outcome = "canceled"
			v.Turns[i].Ended = true
		}
	}
	if clear {
		p := &s.agent.v2.playback
		p.bridgeQueue = slices.DeleteFunc(p.bridgeQueue, func(q bridgeQueued) bool {
			if q.Turn >= floor {
				return false
			}
			if r, ok := b.records[q.Sequence]; ok && r.Payload != nil {
				canceled = append(canceled, q.Sequence)
				old := r
				r.Receipt.Outcome = "canceled"
				r.Receipt.Revision = v.Revision
				b.audioBytes -= len(r.Payload)
				b.audioDuration -= r.Duration
				p.length -= len(r.Payload)
				p.duration -= r.Duration
				_ = s.reserveBridgeMemory(-int64(len(r.Payload)))
				r.Payload = nil
				b.replaceRecordEncoding(&old, &r)
				b.records[q.Sequence] = r
			}
			return true
		})
	}
	advanceBridgeSent(b, v)
	b.state = v
	s.projectBridgeLocked(s.state.Agent.V2)
	// Attempt cancellation is a signal, never a callback wait. The actor also
	// checks the canonical floor before launching/applying a later task.
	for _, t := range b.attempts {
		if cancel && t.turn < floor {
			t.cancel()
		}
	}
	s.wakePacer()
	return canceled
}
func (w *Worker) EndBridgeOutput(scope BridgeOutputScope, turn agent.Turn, last uint64) error {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.bridgeScopeLocked(scope); err != nil {
		return err
	}
	b := s.agent.v2.bridge
	v := cloneBridgeState(b.state)
	i := slices.IndexFunc(v.Turns, func(t bridgeTurn) bool { return t.Turn == turn })
	if i < 0 || last > v.Through {
		return ErrBridgeOutputOrder
	}
	v.Turns[i].Ended = true
	v.Turns[i].Last = last
	v.Revision++
	b.state = v
	p := &s.agent.v2.playback
	if !slices.Contains(p.notify, turn) {
		p.notify = append(p.notify, turn)
	}
	s.projectBridgeLocked(s.state.Agent.V2)
	return nil
}
func (s *session) bridgeTurnBusyLocked(turn agent.Turn) bool {
	b := s.agent.v2.bridge
	for _, t := range b.state.Turns {
		if t.Turn == turn {
			return !t.Ended || t.Last > b.state.Through
		}
	}
	return true
}
func (s *session) bridgeFinishedLocked(turn agent.Turn) {
	b := s.agent.v2.bridge
	v := cloneBridgeState(b.state)
	for i := range v.Turns {
		if v.Turns[i].Turn == turn {
			v.Turns[i].Finished = true
			if v.Turns[i].Outcome == "" {
				v.Turns[i].Outcome = "complete"
			}
		}
	}
	v.Revision++
	b.state = v
}
func (w *Worker) BridgePlaybackOutcome(scope BridgeOutputScope, turn agent.Turn) (BridgePlaybackFinished, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return BridgePlaybackFinished{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.bridgeScopeLocked(scope); err != nil {
		return BridgePlaybackFinished{}, err
	}
	for _, v := range s.agent.v2.bridge.state.Turns {
		if v.Turn == turn && v.Finished {
			return BridgePlaybackFinished{Turn: turn, Outcome: v.Outcome, GapCount: v.Gaps}, nil
		}
	}
	return BridgePlaybackFinished{}, agent.ErrBackpressure
}

// Acknowledgement and replay progress are independent. Neither a timer nor
// local send permits forgetting a digest. The bridge transport advances the
// replay horizon only after old input/work can no longer return.
func (s *session) pruneBridgeLocked() {
	// SaveMoveOnly and checkpoint capture must retain the same exact cut.
	// Acknowledgements still advance durable progress; defer compaction until
	// the next acknowledgement or replay horizon after rollback releases it.
	if s.agent.paused || s.fenced.Load() {
		return
	}
	b := s.agent.v2.bridge
	if b == nil || b.state == nil || b.durable == nil || b.durable.Scope != b.state.Scope {
		return
	}
	defer b.signalChange()
	through := min(b.replayHorizon, b.durable.Sent)
	if q := s.agent.v2.playback.bridgeQueue; len(q) > 0 {
		through = min(through, q[0].Sequence-1)
	}
	if through <= b.state.Pruned && !slices.ContainsFunc(b.state.Turns, func(t bridgeTurn) bool { return t.Finished && t.Last <= through }) {
		return
	}
	v := cloneBridgeState(b.state)
	for seq, r := range b.records {
		if seq <= through && r.Payload == nil {
			b.replaceRecordEncoding(&r, nil)
			delete(b.records, seq)
			_ = s.reserveBridgeMemory(-bridgeRecordCharge)
		}
	}
	s.pruneBridgeSettledLocked(through)
	v.Pruned = max(v.Pruned, through)
	v.Exceptions = slices.DeleteFunc(v.Exceptions, func(r bridgeReceiptRange) bool { return r.Last <= through })
	for i := range v.Exceptions {
		v.Exceptions[i].First = max(v.Exceptions[i].First, through+1)
	}
	v.Turns = slices.DeleteFunc(v.Turns, func(t bridgeTurn) bool { return t.Finished && t.Last <= through })
	v.Revision++
	b.state = v
	s.projectBridgeLocked(s.state.Agent.V2)
}
func (w *Worker) AdvanceBridgeReplayHorizon(scope BridgeOutputScope, through uint64) error {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.bridgeScopeLocked(scope); err != nil {
		return err
	}
	b := s.agent.v2.bridge
	if through > b.state.Through {
		return ErrBridgeOutputOrder
	}
	b.replayHorizon = max(b.replayHorizon, through)
	s.pruneBridgeLocked()
	return nil
}

func (s *session) bridgeCheckpointedLocked(pair agentState, seq uint64) {
	b := s.agent.v2.bridge
	if b == nil || pair.V2 == nil || pair.V2.Bridge == nil || b.state == nil || pair.V2.Bridge.Scope != b.state.Scope {
		return
	}
	if b.durable == nil || pair.V2.Bridge.Revision > b.durable.Revision {
		b.durable = cloneBridgeState(pair.V2.Bridge)
		b.checkpointSeq = seq
	}
	s.pruneBridgeLocked()
}
func (w *Worker) BridgeOutputCheckpointed(scope BridgeOutputScope) ([]byte, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	b := s.agent.v2.bridge
	if err = s.bridgeScopeLocked(scope); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	state, seq := cloneBridgeState(b.durable), b.checkpointSeq
	s.mu.Unlock()
	if state == nil {
		return nil, agent.ErrBackpressure
	}
	if state.Scope != bridgeOutputIdentity(scope) {
		return nil, ErrBridgeOutputUncertain
	}
	return json.Marshal(struct {
		BridgeOutputScope
		Through    uint64               `json:"through_seq"`
		Exceptions []bridgeReceiptRange `json:"exceptions"`
		Revision   uint64               `json:"revision"`
		Checkpoint uint64               `json:"checkpoint_seq"`
	}{scope, state.Through, state.Exceptions, state.Revision, seq})
}

func newBridgeOutputHost(l BridgeOutputLimits) *bridgeOutputHost {
	l = l.Defaults()
	return &bridgeOutputHost{changed: make(chan struct{}, 1), provenUnadmitted: true, attempts: make(map[agent.TaskID]bridgeAttempt), records: make(map[uint64]bridgePacketRecord), ingress: make(chan bridgeOutputRequest, l.PendingPackets), priority: make(chan bridgePriorityRequest, 1), limits: l}
}
func bridgeStateEqual(a, b *bridgeOutputState) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.LastTurn == b.LastTurn && a.LastStream == b.LastStream && a.Version == b.Version && a.Scope == b.Scope && a.Revision == b.Revision && a.Through == b.Through && a.Sent == b.Sent && a.Pruned == b.Pruned && a.CancelFloor == b.CancelFloor && a.ClearFloor == b.ClearFloor && a.RebaseTicks == b.RebaseTicks && a.RebaseThrough == b.RebaseThrough && slices.Equal(a.Exceptions, b.Exceptions) && slices.Equal(a.Turns, b.Turns)
}

func bridgeMetadataCharge(state *bridgeOutputState, playback playbackState) (int, error) {
	data, err := json.Marshal(struct {
		Bridge   *bridgeOutputState `json:"bridge_output/v1,omitempty"`
		Playback playbackState      `json:"pb,omitzero"`
	}{state, playback})
	return len(data), err
}
func appendBridgeQueued(queue []bridgeQueued, q bridgeQueued, rebaseThrough uint64) []bridgeQueued {
	if q.isGap() && len(queue) > 0 {
		last := &queue[len(queue)-1]
		if last.isGap() && last.Last+1 == q.Sequence && last.Turn == q.Turn && (last.Last > rebaseThrough || q.Sequence <= rebaseThrough) {
			last.Last = q.Last
			last.Count += q.Count
			last.Duration += q.Duration
			return queue
		}
	}
	return append(queue, q)
}

// BridgeOutputResume is local reconciliation evidence. A crash checkpoint alone
// cannot prove an absent identity unadmitted; provider starts remain fenced
// until the authenticated bot ceiling is settled by the recovery coordinator.
type BridgeOutputResume struct {
	BridgeOutputScope
	SettledThrough         uint64               `json:"settled_through"`
	DurableThrough         uint64               `json:"durable_through"`
	SentThrough            uint64               `json:"sent_through"`
	PrunedThrough          uint64               `json:"pruned_through"`
	Exceptions             []bridgeReceiptRange `json:"exceptions"`
	Unknown                []bridgeReceiptRange `json:"unknown"`
	NextSequence           uint64               `json:"next_output_seq"`
	RequiresReconciliation bool                 `json:"requires_reconciliation"`
}

func (w *Worker) BridgeOutputResume(scope BridgeOutputScope) (BridgeOutputResume, error) {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return BridgeOutputResume{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.bridgeScopeLocked(scope); err != nil {
		return BridgeOutputResume{}, err
	}
	b := s.agent.v2.bridge
	state := b.state
	out := BridgeOutputResume{BridgeOutputScope: scope, SettledThrough: state.Through, SentThrough: state.Sent, PrunedThrough: state.Pruned, Exceptions: slices.Clone(state.Exceptions), NextSequence: state.Through + 1, RequiresReconciliation: !b.provenUnadmitted}
	if b.durable != nil && b.durable.Scope == state.Scope {
		out.DurableThrough = b.durable.Through
	}
	for _, t := range state.Turns {
		if t.Outcome == "unknown" && t.First != 0 {
			last := t.Last
			if !t.Ended {
				last = state.Through
			}
			first := max(t.First, state.Sent+1)
			if first <= last {
				out.Unknown = append(out.Unknown, bridgeReceiptRange{First: first, Last: last, Turn: t.Turn, Outcome: "unknown"})
			}
		}
	}
	return out, nil
}

// Admission reserves the actual worst encoded floor/end projection, including
// all retained turn markers. A full packet lane cannot consume control space.
func bridgeInterruptionCharge(state *bridgeOutputState, playback playbackState) (int, error) {
	return bridgeInterruptionChargeAt(state, playback, ^uint64(0), ^agent.Turn(0))
}

// Credit bounds valid audio_end by issued plus the whole pending lane. With
// one retained turn, clearing above that turn removes every queued reference;
// clearing at/below it can retain the queue but cannot set a larger clear floor.
// Bound both cases instead of combining maximum floors with an impossible full
// queue. Admission/new-turn checks and the terminal reserve remain unchanged.
func bridgeCreditMetadataCharge(state *bridgeOutputState, playback playbackState, pending int) (int, error) {
	endCeiling := ^uint64(0)
	if n := uint64(pending); state.Through <= endCeiling-n {
		endCeiling = state.Through + n
	}
	if len(state.Turns) != 1 || state.Turns[0].Turn == ^agent.Turn(0) {
		return bridgeInterruptionChargeAt(state, playback, endCeiling, ^agent.Turn(0))
	}
	keep, err := bridgeInterruptionChargeAt(state, playback, endCeiling, max(state.ClearFloor, state.Turns[0].Turn))
	if err != nil {
		return 0, err
	}
	playback.Window = nil
	playback.Pending, playback.Length, playback.Duration = 0, 0, 0
	cleared, err := bridgeInterruptionChargeAt(state, playback, endCeiling, ^agent.Turn(0))
	return max(keep, cleared), err
}

func bridgeInterruptionChargeAt(state *bridgeOutputState, playback playbackState, endCeiling uint64, clearCeiling agent.Turn) (int, error) {
	worst := cloneBridgeState(state)
	worst.CancelFloor = ^agent.Turn(0)
	worst.ClearFloor = clearCeiling
	worst.Revision = ^uint64(0)
	for i := range worst.Turns {
		if !worst.Turns[i].Finished {
			worst.Turns[i].Ended = true
			worst.Turns[i].Outcome = "canceled"
			worst.Turns[i].Last = endCeiling
		}
	}
	return bridgeMetadataCharge(worst, playback)
}

// moveMu protects the split between current actor work and output retention.
// The reserve is a floor, so larger accepted actor work stops output first.
func (s *session) bridgeMemoryLimitLocked() int64 {
	return int64(min(s.agent.v2.registration.Bounds.MaxMailboxBytes, s.snapshotLimits().MoveBytesPerSession)) - max(int64(s.agent.v2.bridge.limits.ActorReserveBytes), s.moveReserved)
}
func (s *session) bridgeCreditExhaustedLocked() bool {
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	return s.agent.v2.bridge.memory.Load()+int64(3*bridgeBinaryBytes+bridgeRecordCharge+64) > s.bridgeMemoryLimitLocked()
}
