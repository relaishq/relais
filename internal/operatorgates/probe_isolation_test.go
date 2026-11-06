package operatorgates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestProbeEvidenceCannotChangePolicyOrReadSuccess(t *testing.T) {
	var baseline []byte
	for _, mode := range []string{"absent", "falsified", "unreadable", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixtureSession(t)
			s.inventoryComponents = map[string][]string{"relay": {"relay-primary", "relay-standby"}, "shared": {"control", "signal"}, "w1": {"w1"}, "w2": {"w2"}}
			file := ""
			if mode != "absent" {
				file = "/private/probe-file"
			}
			rows := []clocks.TrackingRow{}
			for n := int64(-3); n <= 3; n++ {
				rows = append(rows, clocks.TrackingRow{UnixSeconds: 1793930000 + n, MaxErrorSeconds: 0.0005, LeapStatus: "N"})
			}
			peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
				readsProbes := strings.Contains(string(req.Script), "args=json.loads")
				require.Equal(t, file != "" && req.Target == "shared", readsProbes, "only the signal host reads probes")
				sample := map[string]any{"errorBoundNs": 1, "rateErrorPpm": 0.1, "measuredUnixNs": "1793930000000000000", "trackingLog": &clocks.TrackingLog{Rows: rows}}
				if readsProbes {
					batch := clocks.ProbeBatch{Cursor: json.RawMessage(`null`)}
					switch mode {
					case "falsified":
						r := clockprobe.Record{Target: clockprobe.WorkerOne, FrontendSend: 1793930000100000000, FrontendReceive: 1793930000102000000, HostReceive: 1793930000111000000, HostSend: 1793930000111000000}
						line, err := r.MarshalLine()
						require.NoError(t, err)
						batch.Lines = string(line)
						batch.Cursor = json.RawMessage(`{"file":"1:2","position":84,"anchor":"` + strings.Repeat("a", 64) + `","skipLine":false}`)
					case "unreadable":
						batch.LossCounts.ReadFailures = 1
					}
					sample["probeLog"] = batch
					if mode == "malformed" {
						sample["probeLog"] = map[string]any{"lines": 42, "cursor": "private-label"}
					}
				}
				raw, err := json.Marshal(sample)
				require.NoError(t, err)
				return hostReply(t, raw)
			}}
			var observed clocks.Evidence
			runtime := &ProcessRuntime{Session: s, Peer: peer, Config: &ProcessConfig{Parameters: ProcessParameters{ClockProbeFile: file}}, Bounds: &clocks.Bounds{FrontendControlBoundNS: 2, FrontendWorkerBoundNS: 2, DriftPPM: 1}, ClockObserved: func(e clocks.Evidence) { observed = e }}
			require.NoError(t, runtime.VerifyClocks(context.Background(), time.Minute))
			require.False(t, s.Latched())
			require.Equal(t, 4, peer.sent)
			if mode == "absent" {
				require.Nil(t, observed.Falsification)
			} else {
				require.NotNil(t, observed.Falsification)
			}
			if mode == "falsified" {
				require.Equal(t, 1, observed.Falsification.CountsByStatus["falsified"])
			}
			if mode == "unreadable" || mode == "malformed" {
				require.Equal(t, 1, observed.Falsification.LossCounts.ReadFailures)
			}
			public, err := json.Marshal(observed)
			require.NoError(t, err)
			require.NotContains(t, string(public), "private-label")
			if file != "" {
				require.NotContains(t, string(public), file)
			}
			// Compare all policy results, including latch, causes and final verdict.
			policy := struct {
				Final           any
				Control, Worker int64
				Drift           float64
			}{s.Final(nil), observed.Control, observed.Worker, observed.Drift}
			raw, err := json.Marshal(policy)
			require.NoError(t, err)
			if mode == "absent" {
				baseline = raw
			} else {
				require.JSONEq(t, string(baseline), string(raw))
			}
		})
	}
}

func TestClockCacheHitDoesNotTouchSessionState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := fixtureSession(t)
		r := &ProcessRuntime{Session: s, Config: &ProcessConfig{}, Bounds: &clocks.Bounds{}, ClockSamples: []clocks.Evidence{{At: c.Wall()}}}
		s.mu.Lock()
		defer s.mu.Unlock()
		require.NoError(t, r.VerifyClocks(context.Background(), time.Minute))
	})
	// Missing bounds must not access a session, as before optional probes.
	r := &ProcessRuntime{Config: &ProcessConfig{}}
	require.Error(t, r.VerifyClocks(context.Background(), time.Minute))
}

func TestAmbiguousProbeInventoryDoesNotReadAnyFile(t *testing.T) {
	for _, components := range []map[string][]string{nil, {"w1": {"signal"}, "w2": {"signal"}}} {
		s, _ := fixtureSession(t)
		s.clockProbeFile = "/private/probes"
		s.inventoryComponents = components
		peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
			require.NotContains(t, string(req.Script), "args=json.loads")
			return hostReply(t, []byte(`{"errorBoundNs":1,"rateErrorPpm":0.1,"measuredUnixNs":"1793930000000000000"}`))
		}}
		e, err := s.FreshClocks(context.Background(), peer, clocks.Bounds{FrontendControlBoundNS: 2, FrontendWorkerBoundNS: 2, DriftPPM: 1}, time.Minute)
		require.NoError(t, err)
		require.False(t, s.Latched())
		require.Equal(t, "unavailable", e.Falsification.FrontendSlot)
		require.Equal(t, 1, e.Falsification.LossCounts.ReadFailures)
	}
}

func TestConcurrentProbeRefreshesDoNotReplayAPrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := fixtureSession(t)
		s.clockProbeFile = "/private/probes"
		s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}, "w1": {"w1"}, "w2": {"w2"}}
		reached, release := make(chan struct{}), make(chan struct{})
		sharedReads := 0
		peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
			sample := map[string]any{"errorBoundNs": 1, "rateErrorPpm": 0.1, "measuredUnixNs": "1793930000000000000"}
			if req.Target == "shared" && strings.Contains(string(req.Script), "args=json.loads") {
				sharedReads++
				if sharedReads == 1 {
					close(reached)
					<-release
				}
				_, tail, ok := strings.Cut(string(req.Script), "args=json.loads(base64.b64decode('")
				require.True(t, ok)
				encoded, _, ok := strings.Cut(tail, "'")
				require.True(t, ok)
				args, err := base64.StdEncoding.DecodeString(encoded)
				require.NoError(t, err)
				var input struct {
					Probes *struct {
						Position int64 `json:"position"`
					} `json:"probes"`
				}
				require.NoError(t, json.Unmarshal(args, &input))
				batch := clocks.ProbeBatch{Cursor: json.RawMessage(`{"file":"1:2","position":84,"anchor":"` + strings.Repeat("a", 64) + `","skipLine":false}`)}
				if input.Probes == nil || input.Probes.Position == 0 {
					line, err := (clockprobe.Record{Target: clockprobe.Control, FrontendSend: 1793930000000000000, FrontendReceive: 1793930000000001000, Result: clockprobe.RequestFailed}).MarshalLine()
					require.NoError(t, err)
					batch.Lines = string(line)
				}
				sample["probeLog"] = batch
			}
			raw, err := json.Marshal(sample)
			require.NoError(t, err)
			return hostReply(t, raw)
		}}
		done := make(chan error, 2)
		refresh := func() {
			_, err := s.FreshClocks(context.Background(), peer, clocks.Bounds{FrontendControlBoundNS: 2, FrontendWorkerBoundNS: 2, DriftPPM: 1}, time.Minute)
			done <- err
		}
		go refresh()
		<-reached
		secondStarted := make(chan struct{})
		go func() { close(secondStarted); refresh() }()
		<-secondStarted
		// Mutex waits are not durable synctest waits. Release the parked read
		// explicitly after starting the concurrent refresh.
		close(release)
		require.NoError(t, <-done)
		require.NoError(t, <-done)
		e := s.clockProbeHistory.Snapshot("shared", map[string]string{"control": "shared"})
		require.Equal(t, 1, e.CountsByStatus["unchecked"])
		require.Equal(t, 1, e.CountsByReason["request-failed"])
	})
}
