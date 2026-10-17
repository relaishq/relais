package loopback

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestResumeStateEncodingUsesOnlyAppOwnedKeys(t *testing.T) {
	a := New(agent.FactoryArgs{})
	_, err := a.Handle(context.Background(), agent.Event{Kind: agent.Resumed, Resume: &agent.ResumeNotice{ResumeNotice: legacy.ResumeNotice{Kind: legacy.PlannedMove, CheckpointAge: 123 * time.Millisecond, SnapshotAge: 456 * time.Millisecond, InputMayBeDuplicated: true, Progress: legacy.Progress{SSRC: 9, Index: 17, Consumed: 8}, DuplicateWindows: []legacy.DuplicateWindow{{SSRC: 9, First: 1, Last: 17}}}, MoveOnly: agent.OK}})
	require.NoError(t, err)
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	require.JSONEq(t, `{"turn":1,"phase":"recording","cursor":0,"packets":0,"seen":{"ssrc":0,"high":0,"mask":[0,0],"set":false},"resume":{"kind":"planned_move","checkpoint_age_ns":123000000,"snapshot_age_ns":456000000,"input_may_be_duplicated":true,"move_only":"ok"}}`, string(saved.Bytes))
}

func TestRestorePriorResumeSchemaThenSaveOwnedSchema(t *testing.T) {
	a := New(agent.FactoryArgs{})
	old := agent.State{Version: 1, Bytes: []byte(`{"turn":7,"phase":"recording","cursor":0,"packets":0,"seen":{"ssrc":0,"high":0,"mask":[0,0],"set":false},"resume":{"Kind":"takeover","CheckpointAge":123,"SnapshotAge":456,"InputMayBeDuplicated":true,"DuplicateWindows":[{"SSRC":9,"First":1,"Last":17}],"Progress":{"SSRC":9,"Index":17},"MoveOnly":"outcome_unknown","FutureHostField":"must not persist"}}`)}
	require.NoError(t, a.Restore(context.Background(), old))
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, saved.Version)
	require.JSONEq(t, `{"turn":7,"phase":"recording","cursor":0,"packets":0,"seen":{"ssrc":0,"high":0,"mask":[0,0],"set":false},"resume":{"kind":"takeover","checkpoint_age_ns":123,"snapshot_age_ns":456,"input_may_be_duplicated":true,"move_only":"outcome_unknown"}}`, string(saved.Bytes))
}

func TestOtherSSRCDoesNotAdvanceTurn(t *testing.T) {
	a := New(agent.FactoryArgs{})
	_, err := a.Handle(context.Background(), input(1, []byte{0xf8, 0xff, 0xfe}))
	require.NoError(t, err)
	other := input(2, []byte{0xf8, 0xff, 0xfe})
	other.Input.Progress.SSRC = 99
	effects, err := a.Handle(context.Background(), other)
	require.NoError(t, err)
	require.Equal(t, agent.Effects{}, effects)
	saved, err := a.Save(context.Background())
	require.NoError(t, err)
	var state struct {
		Turn    uint64 `json:"turn"`
		Packets uint32 `json:"packets"`
	}
	require.NoError(t, json.Unmarshal(saved.Bytes, &state))
	require.EqualValues(t, 1, state.Turn)
	require.EqualValues(t, 1, state.Packets)
}
