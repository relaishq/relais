package mediaworker

import (
	"bytes"
	"encoding/json"
	"math"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// These are ADR proposals for measurement, not live admission/default changes.
var proposedComponentBudgets = componentBudgets{Hot: 7168, Setup: 4096, History: 8448, Total: 20480}

func maximalComponentSnapshot(t *testing.T) snapshot {
	t.Helper()
	snap := componentFixture(t)
	maxTime := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	snap.State.ICE.RemoteAddr = netip.MustParseAddrPort("[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff%4294967295]:65535")
	snap.State.Checkpoint = CheckpointState{CapturedAt: maxTime, AgeAtCapture: math.MaxInt64, Attempts: math.MaxUint64, Successes: math.MaxUint64, Failures: math.MaxUint64, RTPPacketRate: math.MaxFloat64, SRTCPPacketRate: math.MaxFloat64, TakeoverAge: math.MaxInt64, TakeoverSnapshotAge: math.MaxInt64, TakeoverStoredAt: maxTime}
	snap.State.Agent.Progress = agent.Progress{SSRC: math.MaxUint32, Index: math.MaxUint64, Timestamp: math.MaxUint32, Consumed: math.MaxUint64}
	snap.State.Agent.Revision = math.MaxUint64
	for i, track := range []*trackState{&snap.State.Audio, &snap.State.Video} {
		*track = trackState{ID: track.ID, MID: track.MID, PayloadType: 255, SSRC: math.MaxUint32 - uint32(i), InitialSeq: math.MaxUint16, InitialTS: math.MaxUint32, Anchored: true, InboundSSRC: math.MaxUint32 - uint32(i), SeqOffset: math.MaxUint16, TSOffset: math.MaxUint32, AdvanceSinceSend: math.MaxUint32, Packets: math.MaxUint64, HighestSentIndex: math.MaxUint64, ReplayFloor: math.MaxUint64, LastTimestamp: math.MaxUint32, SRTCPIndex: math.MaxUint32}
	}
	snap.State.SRTP.Inbound = map[uint32]uint64{math.MaxUint32: math.MaxUint64, math.MaxUint32 - 1: math.MaxUint64}
	snap.State.SRTP.Received = map[uint32]uint64{math.MaxUint32: math.MaxUint64, math.MaxUint32 - 1: math.MaxUint64}
	e := snap.State.Agent.V2
	e.ControlHigh, e.ControlFloor = math.MaxUint64, math.MaxUint64-2
	for i := range e.Controls {
		e.Controls[i].Sequence = math.MaxUint64 - uint64(len(e.Controls)-1-i)
	}
	e.HasMoveOnly, e.MoveOnlyLength, e.Started = true, 1<<20, true
	e.CancelFloor, e.TimerGeneration = agentv2.Turn(math.MaxUint64), math.MaxUint64
	e.Quota = &snapshotQuota{Version: 1, Balance: quotaBalance(math.MaxUint64), At: maxTime}
	// The entire host grammar (including registration, timers, intents, cursors,
	// receipts, quota and move-only metadata) shares the documented 2048-byte cap.
	// A padded registration identity reaches that cap without inventing a new
	// per-field limit. Task/Control arrays cannot independently spend 2048 each.
	host, err := json.Marshal(e)
	require.NoError(t, err)
	require.LessOrEqual(t, len(host), 2048)
	e.Name += strings.Repeat("N", 2048-len(host))
	host, err = json.Marshal(e)
	require.NoError(t, err)
	require.Len(t, host, 2048)
	snap.State.Agent.State.Bytes = bytes.Repeat([]byte{0xff}, 3072)
	state, err := decodeDTLSSerializedState(snap.DTLSConnection)
	require.NoError(t, err)
	setup, _, err := splitDTLSComponents(snap.DTLSConnection)
	require.NoError(t, err)
	snap.DTLSConnection, err = joinDTLSComponents(setup, componentDTLSProgress{LocalEpoch: math.MaxUint16, RemoteEpoch: math.MaxUint16, SequenceNumber: 1 << 48})
	require.NoError(t, err)
	require.NotEmpty(t, state.MasterSecret)
	return snap
}

func TestComponentSizeBudgets(t *testing.T) {
	snap := maximalComponentSnapshot(t)
	c, err := splitSessionComponents(snap, bytes.Repeat([]byte{0xff}, 8192), nil)
	require.NoError(t, err)
	master := bytes.Repeat([]byte{0x31}, 32)
	sealed, m, err := sealSessionComponents(master, 255, snap.State.ID, componentMaxMetadata(), c)
	require.NoError(t, err)
	packed, err := m.marshal()
	require.NoError(t, err)
	reserve := len(packed) + 8 + 3 + 78
	require.Equal(t, 384, reserve)
	hot := len(sealed.Transport) + len(sealed.Work) + reserve
	total := hot + len(sealed.Setup) + len(sealed.History)
	for _, row := range []struct {
		name         string
		size, budget int
	}{{"transport+host ciphertext", len(sealed.Transport), 7168 - len(sealed.Work) - reserve}, {"working raw", 3072, 3072}, {"working ciphertext", len(sealed.Work), 3072 + 6 + 30}, {"hot checkpoint", hot, 7168}, {"setup ciphertext (real ECDSA fixture)", len(sealed.Setup), 4096}, {"history raw", 8192, 8192}, {"history ciphertext", len(sealed.History), 8448}, {"reference reserve", reserve, 384}, {"transaction", total, 20480}} {
		t.Logf("%-40s %5d / %5d B", row.name, row.size, row.budget)
		require.LessOrEqual(t, row.size, row.budget, row.name)
	}
	_, err = proposedComponentBudgets.admit(c, componentMaxMetadata())
	require.NoError(t, err)
	// Arbitrarily many authenticated SSRCs and unbounded negotiated text do
	// exist today. No finite global worst-case can honestly be inferred. Prove
	// real encoded-size admission is required instead of claiming two streams
	// are a new enforced maximum. Nothing in this slice changes live admission.
	oversized := snap
	oversized.State.Audio.ID = strings.Repeat("i", 7168)
	tooLarge, err := splitSessionComponents(oversized, nil, nil)
	require.NoError(t, err)
	_, err = proposedComponentBudgets.admit(tooLarge, componentMaxMetadata())
	require.ErrorIs(t, err, agentv2.ErrCapacity)
}

func TestComponentDemoWorstStateFitsWorkingAndHistory(t *testing.T) {
	// Captured from voice.State/Timings/Window using the exact initializer in
	// examples/voice-agent/voice/pipeline_invariants_test.go. Save uses that same
	// json.Marshal. The fixture avoids a forbidden core -> examples dependency.
	raw, err := os.ReadFile("testdata/v10-demo-state.json")
	require.NoError(t, err)
	require.Len(t, raw, 7087)
	var state map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &state))
	history := state["history"]
	require.Len(t, history, 4205)
	delete(state, "history")
	working, err := json.Marshal(state)
	require.NoError(t, err)
	require.Len(t, working, 2871)
	require.LessOrEqual(t, len(working), 3072)
	require.LessOrEqual(t, len(history), 8192)
	snap := maximalComponentSnapshot(t)
	snap.State.Agent.State.Bytes = working
	c, err := splitSessionComponents(snap, history, nil)
	require.NoError(t, err)
	_, err = proposedComponentBudgets.admit(c, componentMaxMetadata())
	require.NoError(t, err)
	t.Logf("Demo A: raw working %d / 3072 B; raw history %d / 8192 B; encoded hot %d / 7168 B", len(working), len(history), len(c.Transport)+len(c.Work)+60+384)
}

func TestComponentEncodedAdmissionBoundary(t *testing.T) {
	snap := maximalComponentSnapshot(t)
	history := bytes.Repeat([]byte{0xff}, 8192)
	c, err := splitSessionComponents(snap, history, nil)
	require.NoError(t, err)
	measured, err := measureComponents(c, componentMaxMetadata())
	require.NoError(t, err)
	snap.State.Audio.ID += strings.Repeat("x", 7168-measured.Hot)
	c, err = splitSessionComponents(snap, history, nil)
	require.NoError(t, err)
	measured, err = proposedComponentBudgets.admit(c, componentMaxMetadata())
	require.NoError(t, err)
	require.Equal(t, 7168, measured.Hot)
	snap.State.Audio.ID += "x"
	c, err = splitSessionComponents(snap, history, nil)
	require.NoError(t, err)
	measured, err = proposedComponentBudgets.admit(c, componentMaxMetadata())
	require.ErrorIs(t, err, agentv2.ErrCapacity)
	require.Equal(t, 7169, measured.Hot)
}
