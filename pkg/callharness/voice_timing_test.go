package callharness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/pkg/agent/opusio"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

// reserveTimingTurn locks the ledger across processes and charges BEFORE warm
// or any paid task. Unparseable monetary entries fail closed. Each attempted
// turn consumes its conservative estimate even when it fails or is interrupted.
func reserveTimingTurn(path string, capUSD float64, worker, turn int) error {
	if path == "" || capUSD <= 0 || math.IsNaN(capUSD) || math.IsInf(capUSD, 0) {
		return fmt.Errorf("timing ledger and finite positive cap required")
	}
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		return fmt.Errorf("timing ledger lock unavailable")
	}
	defer func() { _ = os.Remove(path + ".lock") }()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("timing ledger unavailable")
	}
	total := 0.0
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.LastIndex(line, "$")
		if i < 0 {
			continue
		}
		fields := strings.Fields(line[i+1:])
		if len(fields) != 1 {
			return fmt.Errorf("invalid timing ledger entry")
		}
		n, e := strconv.ParseFloat(fields[0], 64)
		if e != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("invalid timing ledger entry")
		}
		total += n
	}
	const estimate = 0.02
	if total+estimate > capUSD+1e-9 {
		return fmt.Errorf("timing ledger cap reached")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("timing ledger unavailable")
	}
	_, err = fmt.Fprintf(f, "%s | voice timing worker %d turn %d | $0.0200\n", time.Now().UTC().Format(time.RFC3339), worker, turn)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("timing ledger write failed")
	}
	return nil
}
func TestVoiceTimingLedgerRefusesUnbudgetedTurns(t *testing.T) {
	path := t.TempDir() + "/ledger.md"
	require.Error(t, reserveTimingTurn("", 1, 1, 1))
	require.Error(t, reserveTimingTurn(path, math.NaN(), 1, 1))
	require.NoError(t, reserveTimingTurn(path, 0.04, 1, 1))
	require.NoError(t, reserveTimingTurn(path, 0.04, 1, 2))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.ErrorContains(t, reserveTimingTurn(path, 0.04, 2, 1), "cap reached")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, os.WriteFile(path, []byte("bad | $NaN\n"), 0600))
	require.Error(t, reserveTimingTurn(path, 1, 1, 1))
	require.NoError(t, os.Mkdir(path+".lock", 0700))
	require.ErrorContains(t, reserveTimingTurn(path, 1, 1, 1), "lock unavailable")
}

func timingWorkerCount(value string) (int, error) {
	if value == "" {
		return 3, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("positive worker count required")
	}
	return n, nil
}
func TestVoiceTimingWorkerCountSupportsOneColdPair(t *testing.T) {
	n, err := timingWorkerCount("1")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = timingWorkerCount("")
	require.NoError(t, err)
	require.Equal(t, 3, n)
	for _, invalid := range []string{"0", "-1", "bad"} {
		_, err = timingWorkerCount(invalid)
		require.Error(t, err)
	}
}

// TestVoiceAgentLiveTiming runs N fresh pools/workers, two identical fixture
// questions per worker. No fixture generation or env-file credential loading.
// It is keyless by default and adds no work to the short harness.
func TestVoiceAgentLiveTiming(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_VOICE_LIVE_TIMING") != "1" || os.Getenv("OPENAI_API_KEY") == "" || os.Getenv("CI") != "" {
		t.Skip("opt-in local provider timing measurement")
	}
	require.NoError(t, voice.ValidateWarmMode(os.Getenv("RELAIS_VOICE_WARM")))
	ledger := os.Getenv("RELAIS_VOICE_LEDGER")
	require.NotEmpty(t, ledger, "ledger required before any provider call")
	capUSD, err := strconv.ParseFloat(os.Getenv("RELAIS_VOICE_TIMING_CAP_USD"), 64)
	require.NoError(t, err, "explicit dollar cap required")
	require.True(t, capUSD > 0 && !math.IsInf(capUSD, 0) && !math.IsNaN(capUSD))
	n, err := timingWorkerCount(os.Getenv("RELAIS_VOICE_TIMING_WORKERS"))
	require.NoError(t, err)
	fixturePath := os.Getenv("RELAIS_VOICE_LIVE_AUDIO")
	require.NotEmpty(t, fixturePath, "existing question Ogg required")
	fixture, err := os.ReadFile(fixturePath)
	require.NoError(t, err)
	reader, err := opusio.NewReader(bytes.NewReader(fixture))
	require.NoError(t, err)
	var packets [][]byte
	for {
		audio, e := reader.Next(context.Background())
		if e == io.EOF {
			break
		}
		require.NoError(t, e)
		packets = append(packets, audio.Packets...)
	}
	require.NotEmpty(t, packets)
	require.Less(t, len(packets), 250, "fixture must stay under five seconds")
	var requestHistory voice.RequestHistory
	for worker := 1; worker <= n; worker++ {
		t.Run(fmt.Sprintf("worker_%d", worker), func(t *testing.T) {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			provider := voice.NewOpenAIWithClient(os.Getenv("OPENAI_API_KEY"), &http.Client{Transport: transport, Timeout: 40 * time.Second})
			var mu sync.Mutex
			var traces []voice.RequestTrace
			provider.Trace = func(row voice.RequestTrace) { mu.Lock(); defer mu.Unlock(); traces = append(traces, row) }
			reg := exampleRegistration(voice.Registration(provider))
			limits := mediaworker.SnapshotLimits{MaxBytes: 24576, BytesPerSecond: 270336, BurstBytes: 98304}
			sys := startAgentHarness(t, nil, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{limits, limits}, remoteTarget: true, callbackBudget: 2 * time.Second})
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
			defer cancel()
			call, e := sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, e)
			received := receiveVoice(t, call)
			var rows []voice.Measurement
			for turn := 1; turn <= 2; turn++ {
				require.NoError(t, reserveTimingTurn(ledger, capUSD, worker, turn))
				mu.Lock()
				traces = nil
				mu.Unlock()
				voiceControl(t, sys, call.SessionID(), uint64(2*turn-1), "ptt.start")
				for _, packet := range packets {
					require.NoError(t, call.audio.WriteSample(media.Sample{Data: packet, Duration: 20 * time.Millisecond}))
					time.Sleep(20 * time.Millisecond)
				}
				released := time.Now()
				voiceControl(t, sys, call.SessionID(), uint64(2*turn), "ptt.stop")
				var first time.Duration
				for first == 0 {
					select {
					case packet := <-received:
						if !packet.at.Before(released) {
							first = packet.at.Sub(released)
						}
					case <-ctx.Done():
						t.Fatal("first Pion audio deadline")
					}
				}
				var state voice.State
				require.Eventually(t, func() bool {
					var ok bool
					state, ok = voiceState(sys.workers[0], call.SessionID())
					return ok && state.Phase == "idle"
				}, 60*time.Second, 10*time.Millisecond)
				require.Equal(t, agent.OK, state.Outcome, "safe error code: %s", state.Error)
				mu.Lock()
				copied := append([]voice.RequestTrace(nil), traces...)
				mu.Unlock()
				rows = append(rows, voice.Measurement{Worker: worker, Turn: turn, WarmMode: provider.WarmMode(), Timings: state.Timings, FirstAudio: first, Requests: requestHistory.Observe(copied)})
				// Flush each completed turn so a later refusal retains its measurement.
				var table bytes.Buffer
				require.NoError(t, voice.WriteMeasurementTable(&table, rows[len(rows)-1:]))
				t.Log("\n" + table.String())
			}
			_, e = call.Hangup(ctx)
			require.NoError(t, e)
		})
	}
}
