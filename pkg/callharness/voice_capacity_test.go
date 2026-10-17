package callharness

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type voiceStallStore struct {
	sessionstore.Store
	stalled  atomic.Bool
	failures atomic.Int64
}

func (s *voiceStallStore) PutState(ctx context.Context, lease sessionstore.Lease, state []byte) error {
	if s.stalled.Load() {
		s.failures.Add(1)
		return errors.New("fixture: checkpoint write stalled")
	}
	return s.Store.PutState(ctx, lease, state)
}

type capacityProvider struct {
	pipelineProvider
	slowWarm bool
	warms    atomic.Int64
	blocked  atomic.Bool
}

func (p *capacityProvider) Warm(ctx context.Context) {
	if p.warms.Add(1) == 2 && p.slowWarm {
		<-ctx.Done()
	}
}
func (p *capacityProvider) Speech(ctx context.Context, key, text string) (io.ReadCloser, error) {
	if p.slowWarm && text == voice.Segments(pipelineReply)[1] && !p.blocked.Swap(true) {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.pipelineProvider.Speech(ctx, key, text)
}
func TestVoiceAgentCapacityRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("voice capacity network matrix")
	}
	for _, cut := range []string{"store_stall", "slow_warm_after_barge"} {
		t.Run(cut, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				stalled := &voiceStallStore{Store: store}
				// Reuse the real three-segment fixtures; replace only the provider's cuts.
				packets := pacerFixtures(t, 75)
				p := &capacityProvider{slowWarm: cut == "slow_warm_after_barge"}
				p.entered = make(chan struct{}, 2)
				for i := range p.audio {
					encoded := encodeVoicePackets(packets[i*25 : (i+1)*25])
					var err error
					p.audio[i], err = voice.RecordingOgg(encoded)
					require.NoError(t, err)
				}
				reg := exampleRegistration(voice.Registration(p))
				limits := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
				sys := startAgentHarness(t, stalled, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{limits, limits}, remoteTarget: true, callbackBudget: 2 * time.Second})
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				id := call.SessionID()
				received := receiveVoice(t, call)
				voiceControl(t, sys, id, 1, "ptt.start")
				sendLoopPackets(t, call, pacerFixtures(t, 5))
				waitVoice(t, sys.workers[0], id, func(s voice.State) bool {
					return bits.OnesCount64(s.Seen.Mask[0])+bits.OnesCount64(s.Seen.Mask[1]) >= 5
				})
				voiceControl(t, sys, id, 2, "ptt.stop")
				if cut == "store_stall" {
					stalled.stalled.Store(true)
					timer := time.AfterFunc(1100*time.Millisecond, func() { stalled.stalled.Store(false) })
					defer func() { timer.Stop(); stalled.stalled.Store(false) }()
				} else {
					select {
					case <-p.entered:
					case <-ctx.Done():
						t.Fatal("later render did not start")
					}
					waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "playing" && s.Render == 1 })
					voiceControl(t, sys, id, 3, "ptt.start")
					// Discard only packets that the old turn sent before the barge-in.
					drain := time.NewTimer(100 * time.Millisecond)
				draining:
					for {
						select {
						case <-received:
						case <-drain.C:
							break draining
						}
					}
					sendLoopPackets(t, call, pacerFixtures(t, 5))
					waitVoice(t, sys.workers[0], id, func(s voice.State) bool {
						return bits.OnesCount64(s.Seen.Mask[0])+bits.OnesCount64(s.Seen.Mask[1]) >= 10
					})
					voiceControl(t, sys, id, 4, "ptt.stop")
				}
				state := waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "idle" })
				require.Equal(t, agent.OK, state.Outcome, "%s / %s", state.Error, state.Notice)
				if cut == "store_stall" {
					require.Positive(t, stalled.failures.Load())
					for i := range p.calls {
						require.EqualValues(t, 1, p.calls[i].Load(), "completed segment %d must not be billed again", i)
					}
				}
				var got [][]byte
				for len(got) < len(packets) {
					got = append(got, nextPaced(t, received).packet.Payload)
				}
				require.Equal(t, packets, got, "the complete reply must retain packet order")
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}

func encodeVoicePackets(packets [][]byte) []byte {
	b := []byte{1}
	for _, p := range packets {
		b = binary.BigEndian.AppendUint16(b, uint16(len(p)))
		b = append(b, p...)
	}
	return b
}
