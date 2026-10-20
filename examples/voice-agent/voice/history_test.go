package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestHistoryLayoutsAndDefaultBudgets(t *testing.T) {
	ctx := context.Background()
	for _, alphabet := range []string{"x", "\x00\"\\<>&😀汉"} {
		t.Run(alphabet, func(t *testing.T) {
			legacy := worstStateAgent(alphabet)
			// Fill any remainder left by a multi-byte/escaped rune to the exact bounds.
			pad := func(s string, n int) string {
				b, err := json.Marshal(s)
				require.NoError(t, err)
				return s + strings.Repeat("x", n-(len(b)-2))
			}
			legacy.state.Transcript = pad(legacy.state.Transcript, 1024)
			legacy.state.Reply = pad(legacy.state.Reply, MaxReplyBytes)
			for i := range legacy.state.History {
				legacy.state.History[i].User = pad(legacy.state.History[i].User, 512)
				legacy.state.History[i].Assistant = pad(legacy.state.History[i].Assistant, 512)
			}
			inline, err := legacy.Save(ctx)
			require.NoError(t, err)
			expected, err := json.Marshal(legacy.state)
			require.NoError(t, err)
			require.EqualValues(t, 4, inline.Version)
			require.Equal(t, expected, inline.Bytes, "legacy bytes retain exactly the v4 JSON layout")
			require.Empty(t, inline.History)
			require.Equal(t, 7087, len(inline.Bytes))
			split := Registration(nil, true).Factory(agent.FactoryArgs{MaxStateBytes: 3072, MaxHistoryBytes: 8192}).(*Agent)
			split.state = legacy.state
			split.length = legacy.length
			saved, err := split.Save(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 5, saved.Version)
			require.Equal(t, 2871, len(saved.Bytes))
			workingBytes := len(saved.Bytes)
			require.Equal(t, 4205, len(saved.History))
			require.LessOrEqual(t, len(saved.Bytes), 3072)
			require.LessOrEqual(t, len(saved.History), 8192)
			require.NotContains(t, string(saved.Bytes), `"history"`)
			require.True(t, utf8.Valid(saved.History))
			require.Equal(t, legacy.state.History, split.state.History, "Save must not mutate the live history")
			target := Registration(nil, true).Factory(agent.FactoryArgs{MaxStateBytes: 3072, MaxHistoryBytes: 8192}).(*Agent)
			// The size fixture deliberately combines all flags; use a valid playback phase for restore.
			split.state.Phase = "playing"
			saved, err = split.Save(ctx)
			require.NoError(t, err)
			require.NoError(t, target.Restore(ctx, saved))
			require.Equal(t, split.state, target.state)
			again, err := target.Save(ctx)
			require.NoError(t, err)
			require.Equal(t, saved, again)
			require.ErrorIs(t, target.Restore(ctx, inline), agent.ErrVersion)
			require.ErrorIs(t, legacy.Restore(ctx, saved), agent.ErrVersion)
			legacy.state.Phase = "playing"
			legacySaved, err := legacy.Save(ctx)
			require.NoError(t, err)
			// Recovery selects the persisted session profile even when the worker's
			// new-call registration requests split defaults.
			legacyTarget := Registration(nil, true).Factory(agent.FactoryArgs{MaxStateBytes: 8192}).(*Agent)
			require.NoError(t, legacyTarget.Restore(ctx, legacySaved))
			require.Equal(t, legacy.state, legacyTarget.state)
			legacyAgain, err := legacyTarget.Save(ctx)
			require.NoError(t, err)
			require.Equal(t, legacySaved, legacyAgain)
			t.Logf("real Save: legacy=%d, split working=%d/3072 history=%d/8192", len(inline.Bytes), workingBytes, len(saved.History))
		})
	}
	require.Equal(t, agent.Limits{MaxStateBytes: 8192}, Registration(nil).Limits)
	require.Equal(t, agent.Limits{}, Registration(nil, true).Limits)
}

func TestHistoryRestoreBoundsAndRollback(t *testing.T) {
	ctx := context.Background()
	a := Registration(nil, true).Factory(agent.FactoryArgs{MaxStateBytes: 3072, MaxHistoryBytes: 8192}).(*Agent)
	a.state.History = []Exchange{{"prior", "committed"}}
	saved, err := a.Save(ctx)
	require.NoError(t, err)
	a.state.History = append(a.state.History, Exchange{"new", "local"})
	require.NoError(t, a.Restore(ctx, saved))
	require.Equal(t, []Exchange{{"prior", "committed"}}, a.state.History)
	for _, history := range [][]byte{nil, {}, []byte{0xff}, []byte("["), []byte(`[{},{},{},{},{}]`), bytes.Repeat([]byte(" "), 8193)} {
		bad := saved
		bad.History = history
		require.Error(t, a.Restore(ctx, bad))
		after, err := a.Save(ctx)
		require.NoError(t, err)
		require.Equal(t, saved, after)
	}
	a.historyLimit = 2
	_, err = a.Save(ctx)
	require.ErrorIs(t, err, agent.ErrHistoryTooLarge)
}

func TestHistoryEvictsOnlyOldestExchange(t *testing.T) {
	a := Registration(nil, true).Factory(agent.FactoryArgs{MaxStateBytes: 3072, MaxHistoryBytes: 8192}).(*Agent)
	a.state = State{Turn: 10, Phase: "replying", Transcript: "fifth", Reply: "reply", History: []Exchange{{"1", "a"}, {"2", "b"}, {"3", "c"}, {"4", "d"}}}
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Turn: 10, TaskID: "reply", Sequence: 1, Final: true, Outcome: agent.OK}})
	require.NoError(t, err)
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	var history []Exchange
	require.NoError(t, json.Unmarshal(saved.History, &history))
	require.Equal(t, []Exchange{{"2", "b"}, {"3", "c"}, {"4", "d"}, {"fifth", "reply"}}, history)
}
