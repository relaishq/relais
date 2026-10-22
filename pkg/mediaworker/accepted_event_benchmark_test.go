package mediaworker

import (
	"context"
	"net/netip"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// The same fixtures run against the baseline and the accepted-event module.
func BenchmarkAcceptedEventAdmission(b *testing.B) {
	store := sessionstore.NewMemory()
	snap := componentFixture(b)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(b, err)
	s, _ := dtlsCheckpointSession(b, store, snap, lease, ResumeOptions{})
	s.componentMode = true
	prior := cloneAgentState(s.state.Agent)
	candidate := cloneAgentState(prior)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: prior, event: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPlaybackProjectionLocked(b *testing.B) {
	s := &session{agent: &agentHost{v2: &v2Host{}}}
	p := &s.agent.v2.playback
	p.payloads = make(map[audioKey]playbackPayload)
	for i := range 200 {
		key := audioKey{task: "speech", sequence: uint64(i + 1)}
		p.payloads[key] = playbackPayload{audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}, durations: []time.Duration{20 * time.Millisecond}}
		p.queue = append(p.queue, agent.Audio{Turn: 1, Source: agent.AudioRef{TaskID: key.task, Sequence: key.sequence}})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s.mu.Lock()
		projection := s.projectedPlaybackLocked(agent.Effects{}, agent.Event{}).snapshot()
		s.mu.Unlock()
		if projection.Pending != 200 {
			b.Fatal("lost playback")
		}
	}
}
