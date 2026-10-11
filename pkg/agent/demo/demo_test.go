package demo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relais/pkg/agent"
	"github.com/stretchr/testify/require"
)

func input(n uint64) agent.Input {
	return agent.Input{Progress: agent.Progress{SSRC: 123, Index: n, Timestamp: uint32(n * FrameSamples), Consumed: n}, Payload: []byte{0xf8, 0xff, 0xfe}}
}
func savedState(t *testing.T, a *Agent) State {
	t.Helper()
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	state, err := Decode(saved)
	require.NoError(t, err)
	return state
}
func TestDeterministicStateAndAudio(t *testing.T) {
	ctx := context.Background()
	first, second := New(), New()
	for n := uint64(1); n <= 550; n++ {
		want, err := first.Process(ctx, input(n))
		require.NoError(t, err)
		got, err := second.Process(ctx, input(n))
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.True(t, got.Changed)
		require.NotEmpty(t, got.Audio)
		saved, err := second.Save(ctx)
		require.NoError(t, err)
		require.Less(t, len(saved.Bytes), agent.DefaultMaxStateBytes)
		second = New()
		require.NoError(t, second.Restore(ctx, saved))
		require.Equal(t, savedState(t, first), savedState(t, second))
		if n%25 == 0 {
			require.Equal(t, n/25+1, savedState(t, first).Position.Count)
		}
	}
}
func TestResumeKeepsTonePhaseAndChimeState(t *testing.T) {
	for _, kind := range []agent.ResumeKind{agent.PlannedMove, agent.Takeover} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			a := New()
			for n := uint64(1); n <= 37; n++ {
				_, err := a.Process(ctx, input(n))
				require.NoError(t, err)
			}
			saved, err := a.Save(ctx)
			require.NoError(t, err)
			b := New()
			require.NoError(t, b.Restore(ctx, saved))
			notice := agent.ResumeNotice{Kind: kind, CheckpointAge: 123 * time.Millisecond, SnapshotAge: 128 * time.Millisecond, Progress: input(37).Progress}
			require.NoError(t, a.Resume(ctx, notice))
			require.NoError(t, b.Resume(ctx, notice))
			require.Equal(t, Position{Count: 2, PhaseSamples: 12 * FrameSamples}, savedState(t, b).LastResume.Position)
			for n := uint64(38); n < 100; n++ {
				want, err := a.Process(ctx, input(n))
				require.NoError(t, err)
				got, err := b.Process(ctx, input(n))
				require.NoError(t, err)
				require.Equal(t, want, got)
				if n < 43 {
					require.Equal(t, tones[10][n-38], got.Audio)
				}
				saved, err = b.Save(ctx)
				require.NoError(t, err)
				b = New()
				require.NoError(t, b.Restore(ctx, saved))
			}
		})
	}
}
func TestReplayUsesSavedFloorNotWholeDuplicateWindow(t *testing.T) {
	ctx := context.Background()
	a := New()
	for n := uint64(1); n <= 30; n++ {
		_, err := a.Process(ctx, input(n))
		require.NoError(t, err)
	}
	original := savedState(t, a).Position
	require.NoError(t, a.Resume(ctx, agent.ResumeNotice{Kind: agent.Takeover, Progress: input(30).Progress, InputMayBeDuplicated: true, DuplicateWindows: []agent.DuplicateWindow{{SSRC: 123, First: 25, Last: 40}}}))
	for n := uint64(25); n <= 30; n++ {
		out, err := a.Process(ctx, input(n))
		require.NoError(t, err)
		require.Nil(t, out.Audio)
		require.Equal(t, original, savedState(t, a).Position)
		saved, err := a.Save(ctx)
		require.NoError(t, err)
		a = New()
		require.NoError(t, a.Restore(ctx, saved))
	}
	require.EqualValues(t, 6, savedState(t, a).ReplaySuppressed)
	for n := uint64(31); n <= 40; n++ {
		out, err := a.Process(ctx, input(n))
		require.NoError(t, err)
		require.NotNil(t, out.Audio)
	}
	require.Equal(t, Position{Count: 2, PhaseSamples: 15 * FrameSamples}, savedState(t, a).Position)
	// A repeated known packet remains suppressed without trusting a flag.
	out, err := a.Process(ctx, input(35))
	require.NoError(t, err)
	require.Nil(t, out.Audio)
	require.Equal(t, Position{Count: 2, PhaseSamples: 15 * FrameSamples}, savedState(t, a).Position)
	require.NoError(t, a.Resume(ctx, agent.ResumeNotice{Kind: agent.PlannedMove, Progress: savedState(t, a).LastInput}))
}
func TestInputDurationAndRepacketization(t *testing.T) {
	for _, tc := range []struct {
		packet []byte
		frames int
	}{{[]byte{0xf8, 1}, 1}, {[]byte{0xf9, 1, 2}, 2}, {[]byte{0xfb, 3, 1, 2, 3}, 3}, {[]byte{0xfb, 6, 1}, 6}, {[]byte{0x18, 1}, 3}, {[]byte{0x88, 1}, 0}, {[]byte{0xfb, 0}, 0}, {nil, 0}} {
		frames, err := inputFrames(tc.packet)
		if tc.frames == 0 {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.frames, frames)
		a := New()
		out, err := a.Process(context.Background(), agent.Input{Payload: tc.packet})
		require.NoError(t, err)
		n, err := inputFrames(out.Audio)
		require.NoError(t, err)
		require.Equal(t, frames, n)
		require.EqualValues(t, frames*FrameSamples, savedState(t, a).Position.PhaseSamples)
	}
}
func TestStateAndResumeRejectInvalidData(t *testing.T) {
	ctx := context.Background()
	a := New()
	require.ErrorIs(t, a.Restore(ctx, agent.State{Version: 99}), agent.ErrVersion)
	for _, data := range []string{`{}`, `garbage`, `{"position":{"count":1,"phase_samples":24000}}`, `{"position":{"count":1,"phase_samples":1}}`} {
		require.Error(t, a.Restore(ctx, agent.State{Version: 1, Bytes: []byte(data)}))
	}
	_, err := a.Process(ctx, input(1))
	require.NoError(t, err)
	require.Error(t, a.Resume(ctx, agent.ResumeNotice{Kind: agent.Takeover, Progress: input(2).Progress}))
	require.Error(t, a.Resume(ctx, agent.ResumeNotice{Kind: "unknown"}))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before := savedState(t, a)
	_, err = a.Process(cancelled, input(2))
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, savedState(t, a))
	require.ErrorIs(t, a.Resume(cancelled, agent.ResumeNotice{}), context.Canceled)
	_, err = a.Save(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, a.Restore(cancelled, saved), context.Canceled)
	data, err := json.Marshal(struct {
		Version int
		State   any
	}{7, struct{ Agent any }{struct {
		State    agent.State
		Progress agent.Progress
	}{saved, input(1).Progress}}})
	require.NoError(t, err)
	evidence, err := FromSnapshot(data)
	require.NoError(t, err)
	require.Equal(t, before.Position, evidence.Position)
	require.NotContains(t, string(mustJSON(t, evidence)), "Bytes")
}
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
