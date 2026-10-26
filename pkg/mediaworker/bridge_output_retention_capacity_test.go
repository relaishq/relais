package mediaworker

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// No pruning (store paused). Keep `queued` unsent packets as a jitter buffer,
// with full working/history and the max actor envelope. How many retained
// receipts before admission refuses? 29 s at 50 packets/s needs ~1,450.
func TestBridgeOutputStoreStallCapacityCorners(t *testing.T) {
	for _, size := range []int{160, 1200} {
		for _, long := range []bool{false, true} {
			for _, queued := range []int{1, 5} {
				t.Run(fmt.Sprint(size, "-long", long, "-q", queued), func(t *testing.T) {
					rig, scope := newBridgeRig(t, false)
					s := rig.s
					s.agent.v2.bridge.limits.AdmissionTimeout = time.Hour
					go func() {
						for {
							select {
							case <-s.ctx.Done():
								return
							case <-rig.wire.packets:
							}
						}
					}()
					if long {
						scope.BotInstance = strings.Repeat("x", 64)
						scope.LineageID = strings.Repeat("y", 64)
						s.agent.v2.bridge.state = nil
						if err := s.worker.BindBridgeOutput(scope); err != nil {
							t.Fatal(err)
						}
						if _, err := s.worker.BeginBridgeOutput(scope, 1); err != nil {
							t.Fatal(err)
						}
					}
					rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
					rig.app.state.History = make([]byte, 8192)
					if _, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive); err != nil {
						t.Fatal(err)
					}
					var seq uint64
					for seq = 1; seq < 3000; seq++ {
						r := bridgeRequest(scope, seq, time.Now())
						r.data = bridgePacket(size)
						r.data[5] = byte(seq)
						r.digest = sha256.Sum256(r.data)
						if got := s.admitBridgePacket(r); got.Outcome != "admitted" {
							break
						}
						if seq > uint64(queued) {
							paceBridge(t, s)
						}
					}
					s.mu.Lock()
					b := s.agent.v2.bridge
					retained := b.state.Through - b.state.Pruned
					t.Logf("size=%d long=%v queued=%d stopped at seq=%d retained=%d (%.1f s) cost=%d budget=%d ranges=%d", size, long, queued, seq, retained, float64(retained-uint64(queued))*0.02, s.bridgeExportCostLocked(), s.bridgeLookaheadBudget(), len(b.settled))
					s.mu.Unlock()
					if retained-uint64(queued) < 1650 {
						t.Errorf("capacity below 33 s: sent=%d", retained-uint64(queued))
					}
				})
			}
		}
	}
}

func TestBridgeOutputStoreStallAllScopeTokens(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	oldID := s.id
	s.id = strings.Repeat("s", 64)
	s.state.ID = s.id
	s.state.ICE.LocalUfrag = s.id
	delete(s.worker.sessions, oldID)
	s.worker.sessions[s.id] = s
	scope.SessionID = s.id
	scope.BotInstance = strings.Repeat("b", 64)
	scope.LineageID = strings.Repeat("l", 64)
	s.agent.v2.bridge.state = nil
	if err := s.worker.BindBridgeOutput(scope); err != nil {
		t.Fatal(err)
	}
	if _, err := s.worker.BeginBridgeOutput(scope, 1); err != nil {
		t.Fatal(err)
	}
	rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
	rig.app.state.History = make([]byte, 8192)
	if _, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive); err != nil {
		t.Fatal(err)
	}
	s.agent.v2.bridge.limits.AdmissionTimeout = time.Hour
	const queued = 5
	var retained uint64
	for seq := uint64(1); seq < 3000; seq++ {
		r := bridgeRequest(scope, seq, time.Now())
		r.data = bridgePacket(1200)
		r.digest = sha256.Sum256(r.data)
		if got := s.admitBridgePacket(r); got.Outcome != "admitted" {
			retained = seq - 1
			break
		}
		if seq > queued {
			paceBridge(t, s)
			<-rig.wire.packets
		}
	}
	if retained-queued < 1650 {
		t.Fatalf("all 64-byte tokens: sent=%d below 1650", retained-queued)
	}
	t.Logf("all_tokens=64 packet=1200 queued=5 retained=%d sent=%d seconds=%.2f modeled=%d", retained, retained-queued, float64(retained-queued)*0.02, s.bridgeExportCostLocked())
}

// The outage guarantee covers one running turn; retained turn markers remain
// bounded until a durable acknowledgement and replay horizon allow pruning.
func TestBridgeOutputStoreStallRetainedTurnLimit(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	for turn := agent.Turn(1); turn <= 5; turn++ {
		if turn > 1 {
			if _, err := rig.s.worker.BeginBridgeOutput(scope, turn); err != nil {
				t.Fatalf("begin turn %d: %v", turn, err)
			}
		}
		r := bridgeRequest(scope, uint64(turn), time.Now())
		r.turn = turn
		if got := rig.s.admitBridgePacket(r); got.Outcome != "admitted" {
			t.Fatalf("turn %d admission: %+v", turn, got)
		}
		paceBridge(t, rig.s)
		<-rig.wire.packets
		if err := rig.s.worker.EndBridgeOutput(scope, turn, uint64(turn)); err != nil {
			t.Fatal(err)
		}
		rig.s.mu.Lock()
		rig.s.bridgeFinishedLocked(turn)
		rig.s.mu.Unlock()
	}
	if _, err := rig.s.worker.BeginBridgeOutput(scope, 6); !errors.Is(err, agent.ErrBackpressure) {
		t.Fatalf("sixth retained turn must backpressure: %v", err)
	}
}
