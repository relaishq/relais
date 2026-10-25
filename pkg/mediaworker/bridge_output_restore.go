package mediaworker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Freeze is local and non-destructive. Bytes are immutable after admission;
// this image owns its copies and remains valid after later receipt retirement.
func (s *session) exportBridgeLocked() *bridgeOutputMove {
	b := s.agent.v2.bridge
	if b == nil || b.state == nil {
		return nil
	}
	out := &bridgeOutputMove{CreditRevision: b.creditRevision.Load(), Uncertain: !b.provenUnadmitted, Version: bridgeOutputVersion, State: cloneBridgeState(b.state), Records: make([]bridgePacketRecord, 0, len(b.records)), Queue: slices.Clone(s.agent.v2.playback.bridgeQueue), ReplayHorizon: b.replayHorizon, Durable: cloneBridgeState(b.durable), CheckpointSequence: b.checkpointSeq}
	for _, r := range b.records {
		// Payload bytes are immutable. The frozen image owns a reference;
		// live retirement removes only its own reference.
		out.Records = append(out.Records, r)
	}
	slices.SortFunc(out.Records, func(a, b bridgePacketRecord) int {
		if a.Receipt.Sequence < b.Receipt.Sequence {
			return -1
		}
		if a.Receipt.Sequence > b.Receipt.Sequence {
			return 1
		}
		return 0
	})
	return out
}
func (s *session) restoreBridgeOutput(move *movePayload) error {
	b := s.agent.v2.bridge
	saved := s.agent.latest.V2.Bridge
	if b == nil {
		if saved != nil || (move != nil && move.Bridge != nil) {
			return agent.ErrIncompatibleAgent
		}
		return nil
	}
	if saved == nil {
		if move != nil && move.Bridge != nil {
			return agent.ErrIncompatibleAgent
		}
		return nil
	}
	if saved.Version != bridgeOutputVersion || !saved.Scope.valid(s.id) || saved.Sent > saved.Through || saved.Pruned > saved.Sent || len(saved.Exceptions) > b.limits.ReceiptRanges || bridgeRangesBytes(saved.Exceptions) > b.limits.ReceiptBytes {
		return fmt.Errorf("%w: bridge output checkpoint", agent.ErrIncompatibleAgent)
	}
	encodedRanges, err := json.Marshal(saved.Exceptions)
	if err != nil || len(encodedRanges) > b.limits.ReceiptBytes {
		return agent.ErrCapacity
	}
	seenTurns := map[agent.Turn]bool{}
	for _, t := range saved.Turns {
		if seenTurns[t.Turn] || t.Stream == 0 || t.Stream > saved.LastStream || t.Turn > saved.LastTurn || t.First > saved.Through+1 || t.Last > saved.Through {
			return agent.ErrIncompatibleAgent
		}
		switch t.Outcome {
		case "", "complete", "gapped", "canceled", "unknown":
		default:
			return agent.ErrIncompatibleAgent
		}
		seenTurns[t.Turn] = true
	}
	for i, r := range saved.Exceptions {
		if !bridgeReceiptReason(r.Reason) || r.First == 0 || r.Last < r.First || r.Last > saved.Through || (i > 0 && saved.Exceptions[i-1].Last >= r.First) || (r.Outcome != "refused" && r.Outcome != "canceled" && r.Outcome != "unknown") {
			return agent.ErrIncompatibleAgent
		}
	}
	b.state = cloneBridgeState(saved)
	if move == nil {
		b.provenUnadmitted = false
		b.durable = cloneBridgeState(saved)
		b.checkpointSeq = s.state.Checkpoint.Successes
	}
	if move == nil {
		// Crash metadata proves no payload. Do not create a packet under an old
		// identity. The authenticated reconnect must reconcile its missing tail.
		for i := range b.state.Turns {
			if !b.state.Turns[i].Finished {
				b.state.Turns[i].Outcome = "unknown"
			}
		}
		return nil
	}
	image := move.Bridge
	if image == nil || image.Version != bridgeOutputVersion || !bridgeStateEqual(image.State, saved) || image.ReplayHorizon > saved.Through {
		return fmt.Errorf("%w: bridge exact cut", agent.ErrIncompatibleAgent)
	}
	if saved.Through-saved.Pruned != uint64(len(image.Records)) {
		return fmt.Errorf("%w: incomplete exact bridge ledger", agent.ErrIncompatibleAgent)
	}
	if int64(len(image.Records))*bridgeRecordCharge > int64(s.agent.v2.registration.Bounds.MaxMailboxBytes) {
		return agent.ErrCapacity
	}
	if image.Durable != nil && (image.Durable.Scope != saved.Scope || image.Durable.Revision > saved.Revision || image.Durable.Through > saved.Through) {
		return agent.ErrIncompatibleAgent
	}
	b.creditRevision.Store(image.CreditRevision)
	b.provenUnadmitted = !image.Uncertain
	b.durable = cloneBridgeState(image.Durable)
	b.checkpointSeq = image.CheckpointSequence
	records := make(map[uint64]bridgePacketRecord, len(image.Records))
	var memory int64
	audioBytes := 0
	var duration time.Duration
	for _, r := range image.Records {
		seq := r.Receipt.Sequence
		zeroReceipt := r.Receipt.Outcome == "canceled" || (r.Receipt.Outcome == "refused" && (r.Receipt.Reason == "invalid_audio" || r.Receipt.Reason == "closed_turn"))
		if !r.Receipt.Consumed || r.Receipt.Status != "" || r.Receipt.BridgeOutputScope != r.Scope || !bridgeReceiptReason(r.Receipt.Reason) || r.Duration < 0 || r.Duration > 120*time.Millisecond || r.Duration%(2500*time.Microsecond) != 0 || r.Scope != BridgeOutputScope(saved.Scope) || seq <= saved.Pruned || seq > saved.Through || r.Receipt.Revision > saved.Revision || r.Length < 0 || r.Length > bridgeBinaryBytes || ((r.Length == 0 || r.Duration == 0) && !zeroReceipt) || (r.Length == 0 && r.Duration > 0) {
			return agent.ErrIncompatibleAgent
		}
		if _, exists := records[seq]; exists {
			return fmt.Errorf("%w: duplicate exact bridge packet", agent.ErrIncompatibleAgent)
		}
		marker := slices.IndexFunc(saved.Turns, func(t bridgeTurn) bool { return t.Turn == r.Receipt.Turn && t.Stream == r.Stream })
		if marker >= 0 && saved.Turns[marker].First == 0 && r.Receipt.Reason != "closed_turn" {
			return agent.ErrIncompatibleAgent
		}
		// A finished/pruned marker need not survive for a retained digest. Unsent
		// packets always require their exact turn-to-stream binding.
		if len(r.Payload) > 0 {
			d, err := opusDuration(r.Payload)
			if err != nil || d != r.Duration || len(r.Payload) != r.Length || sha256.Sum256(r.Payload) != r.Digest || r.Receipt.Outcome != "admitted" || marker < 0 {
				return fmt.Errorf("%w: exact bridge payload", agent.ErrIncompatibleAgent)
			}
			audioBytes += len(r.Payload)
			duration += d
		} else if r.Receipt.Outcome == "admitted" {
			return fmt.Errorf("%w: missing exact bridge payload", agent.ErrIncompatibleAgent)
		}
		switch r.Receipt.Outcome {
		case "admitted", "sent", "refused", "canceled", "unknown":
		default:
			return agent.ErrIncompatibleAgent
		}
		r.Payload = slices.Clone(r.Payload)
		records[seq] = r
		memory += int64(bridgeRecordCharge + len(r.Payload))
	}
	seen := map[uint64]bool{}
	var previous uint64
	for _, q := range image.Queue {
		if (q.Kind != "bridge" && q.Kind != "gap") || q.Offset != 0 || q.Count != uint32(q.Last-q.Sequence+1) || q.Sequence <= previous || q.Sequence == 0 || q.Last < q.Sequence || q.Last > saved.Through || q.Duration <= 0 {
			return agent.ErrIncompatibleAgent
		}
		previous = q.Last
		var d time.Duration
		for seq := q.Sequence; seq <= q.Last; seq++ {
			r, ok := records[seq]
			if !ok || seen[seq] || r.Stream != q.Stream || r.Receipt.Turn != q.Turn || (!q.isGap() && (q.Last != q.Sequence || r.Receipt.Outcome != "admitted")) || (q.isGap() && (r.Receipt.Outcome != "refused" && r.Receipt.Outcome != "canceled")) {
				return fmt.Errorf("%w: exact bridge reference", agent.ErrIncompatibleAgent)
			}
			seen[seq] = true
			d += r.Duration
		}
		if d != q.Duration {
			return agent.ErrIncompatibleAgent
		}
	}
	for seq, r := range records {
		if r.Payload != nil && !seen[seq] {
			return fmt.Errorf("%w: orphan exact bridge payload", agent.ErrIncompatibleAgent)
		}
	}
	if audioBytes > b.limits.AudioBytes || audioBytes > s.agent.v2.registration.Bounds.MaxAudioBytes || duration > b.limits.AudioDuration {
		return agent.ErrCapacity
	}
	if err := s.reserveBridgeMemory(memory); err != nil {
		return err
	}
	for _, r := range records {
		b.replaceRecordEncoding(nil, &r)
	}
	b.records = records
	b.audioBytes = audioBytes
	b.audioDuration = duration
	b.replayHorizon = image.ReplayHorizon
	p := &s.agent.v2.playback
	p.bridgeQueue = slices.Clone(image.Queue)
	p.length = audioBytes
	p.duration = duration
	return nil
}

func bridgeReceiptReason(reason string) bool {
	switch reason {
	case "", "output_late", "backpressure", "stale_scope", "conflict", "closed_turn", "invalid_audio":
		return true
	default:
		return false
	}
}
