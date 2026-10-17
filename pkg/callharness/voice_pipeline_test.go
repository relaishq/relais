package callharness

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/bits"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

const pipelineReply = "The first sentence has enough words. The second sentence also has enough words. The third sentence finishes the answer."

type pipelineProvider struct {
	voiceFixtureProvider
	audio       [3][]byte
	calls       [3]atomic.Int64
	blockSecond bool
}

func (p *pipelineProvider) Reply(context.Context, string, string, []voice.Exchange) (string, error) {
	p.llm.Add(1)
	return pipelineReply, nil
}
func (p *pipelineProvider) Speech(ctx context.Context, _, text string) (io.ReadCloser, error) {
	for i, segment := range voice.Segments(pipelineReply) {
		if text != segment {
			continue
		}
		p.calls[i].Add(1)
		if i == 1 && p.blockSecond {
			select {
			case p.entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return io.NopCloser(bytes.NewReader(p.audio[i])), nil
	}
	return nil, &voice.ProviderError{Code: "provider_refused"}
}

func pipelineSystem(t *testing.T, store sessionstore.Store, block bool) (*agentHarnessSystem, [2]*pipelineProvider, [][]byte) {
	t.Helper()
	packets := pacerFixtures(t, 75)
	var providers [2]*pipelineProvider
	var regs []agenthost.Registration
	for i := range providers {
		p := &pipelineProvider{voiceFixtureProvider: voiceFixtureProvider{entered: make(chan struct{}, 2)}, blockSecond: i == 0 && block}
		for index := range p.audio {
			encoded := []byte{1}
			for _, packet := range packets[index*25 : (index+1)*25] {
				encoded = binary.BigEndian.AppendUint16(encoded, uint16(len(packet)))
				encoded = append(encoded, packet...)
			}
			var err error
			p.audio[index], err = voice.RecordingOgg(encoded)
			require.NoError(t, err)
		}
		providers[i] = p
		regs = append(regs, exampleRegistration(voice.Registration(p)))
	}
	limits := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
	return startAgentHarness(t, store, agentHarnessSettings{v2: regs, snapshotLimits: []mediaworker.SnapshotLimits{limits, limits}, remoteTarget: true, callbackBudget: 2 * time.Second}), providers, packets
}

func TestVoiceAgentSegmentPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("voice segment network matrix")
	}
	for _, cut := range []string{"full", "move_rendering", "move_playing", "takeover_rendering", "takeover_playing", "barge"} {
		t.Run(cut, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				blocked := cut == "move_rendering" || cut == "takeover_rendering"
				sys, p, want := pipelineSystem(t, store, blocked)
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
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
				var got [][]byte
				if blocked {
					select {
					case <-p[0].entered:
					case <-ctx.Done():
						t.Fatal("second segment did not start rendering")
					}
					waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "playing" && s.Segment == 0 && s.Render == 1 })
				} else if cut != "full" {
					// Observe actual middle-segment packets before the cut, not just its task.
					for len(got) < 30 {
						got = append(got, nextPaced(t, received).packet.Payload)
					}
					waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "playing" && s.Segment == 1 })
				}
				target := 0
				switch cut {
				case "move_rendering", "move_playing":
					_, err = sys.plane.Move(ctx, id, "1")
					require.NoError(t, err)
					target = 1
				case "takeover_rendering", "takeover_playing":
					// Pin the actual segment in the durable checkpoint; an earlier
					// playing snapshot would not establish this takeover cut.
					segment := 1
					if blocked {
						segment = 0
					}
					require.Eventually(t, func() bool {
						b, e := store.GetState(ctx, id)
						if e != nil {
							return false
						}
						var snapshot v2HarnessSnapshot
						if json.Unmarshal(b, &snapshot) != nil {
							return false
						}
						var saved voice.State
						return json.Unmarshal(snapshot.State.Agent.State.Bytes, &saved) == nil && saved.Phase == "playing" && saved.Segment == segment
					}, 3*time.Second, 5*time.Millisecond)
					require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
					state := waitVoice(t, sys.workers[1], id, func(s voice.State) bool { return s.Phase == "idle" && s.Outcome == agent.OutcomeUnknown })
					require.NotEmpty(t, state.Notice)
					for i := range 3 {
						require.Zero(t, p[1].calls[i].Load())
					}
					require.Equal(t, pipelineReply, state.Reply)
					voiceControl(t, sys, id, 3, "ptt.start")
					waitVoice(t, sys.workers[1], id, func(s voice.State) bool { return s.Phase == "recording" })
				case "barge":
					voiceControl(t, sys, id, 3, "ptt.start")
					waitVoice(t, sys.workers[0], id, func(s voice.State) bool { return s.Phase == "recording" })
					// Drain bounded packets already sent before the cancellation was accepted.
					timer := time.NewTimer(200 * time.Millisecond)
				drain:
					for {
						select {
						case <-received:
						case <-timer.C:
							break drain
						}
					}
					select {
					case <-received:
						t.Fatal("old segment audio continued after barge-in")
					case <-time.After(150 * time.Millisecond):
					}
				}
				if cut == "full" || cut == "move_rendering" || cut == "move_playing" {
					for len(got) < len(want) {
						got = append(got, nextPaced(t, received).packet.Payload)
					}
					require.Equal(t, want, got, "segments must retain exact packet order across move")
					state := waitVoice(t, sys.workers[target], id, func(s voice.State) bool { return s.Phase == "idle" })
					require.Equal(t, agent.OK, state.Outcome)
					for i := range 3 {
						require.EqualValues(t, 1+boolInt(blocked && i == 1), p[0].calls[i].Load()+p[1].calls[i].Load(), "completed segments must never be rendered again")
					}
					require.EqualValues(t, 1, p[0].stt.Load()+p[1].stt.Load())
					require.EqualValues(t, 1, p[0].llm.Load()+p[1].llm.Load())
					select {
					case <-received:
						t.Fatal("duplicate or extra segment packet")
					case <-time.After(100 * time.Millisecond):
					}
				}
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}
