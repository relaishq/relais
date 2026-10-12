package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
)

func TestStallIntervalsRecover(t *testing.T) {
	h, err := callharness.Start(callharness.Options{Relay: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	meter := &Meter{}
	var once sync.Once
	c, err := h.Dial(ctx, callharness.CallOptions{Worker: -1, SendPacing: meter.ObservePacing, SendTiming: func(kind string, scheduled, actual time.Time) {
		meter.Observe(kind, scheduled, actual)
		once.Do(func() { time.Sleep(60 * time.Millisecond) })
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.SendMedia(ctx, 2*time.Second) }()
	time.Sleep(700 * time.Millisecond)
	stalled := meter.sample()
	if stalled.intervalPacing.SkippedFrames == 0 {
		t.Fatal("stall did not count skipped frames")
	}
	time.Sleep(time.Second)
	recovered := meter.sample()
	stats := Stats{Lateness: recovered.all, IntervalLateness: recovered.interval, IntervalPacing: recovered.intervalPacing}
	if recovered.interval.Packets < 35 || len(DefaultLimits().Causes(stats)) != 0 {
		t.Fatalf("later interval remained invalid: %+v, causes=%v", stats, DefaultLimits().Causes(stats))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := c.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSkippedFramesInvalidateOnlyTheirInterval(t *testing.T) {
	meter := &Meter{}
	at := time.Now()
	meter.Observe("audio", at, at)
	meter.ObservePacing("audio", 98, 2)
	stalled := meter.sample()
	stats := Stats{Lateness: stalled.all, IntervalPacing: stalled.intervalPacing}
	if causes := DefaultLimits().Causes(stats); len(causes) != 1 || !strings.Contains(causes[0], "skipped frames") {
		t.Fatal(causes)
	}
	meter.ObservePacing("audio", 100, 0)
	recovered := meter.sample()
	stats.IntervalPacing = recovered.intervalPacing
	if len(DefaultLimits().Causes(stats)) != 0 || recovered.pacing.SkippedFrames != 2 {
		t.Fatalf("interval did not recover: %+v", recovered)
	}
	var data bytes.Buffer
	w := NewWriter(&data)
	w.Write(Message{Type: "stats", PID: 42, Stats: &Stats{Lateness: stalled.all, IntervalPacing: stalled.intervalPacing}})
	w.Write(Message{Type: "stats", PID: 42, Stats: &stats})
	w.Write(Message{Type: "call", PID: 42, Call: &callharness.Report{}})
	w.Write(Message{Type: "done", PID: 42})
	p, err := ReadProcess(&data, 42, 1, DefaultLimits())
	if err != nil || !p.Saturated || p.PeakSkippedPercent != 2 || len(p.Causes) != 1 {
		t.Fatalf("lost earlier saturation: %+v %v", p, err)
	}
}

func TestMissingTakeoverRingRecordsMakeRunInconclusive(t *testing.T) {
	at := time.Now()
	e := EventResult{Event: Event{After: 5 * time.Second, Kind: "kill", Worker: 0}, At: at.Add(5 * time.Second), FinishedAt: at.Add(6 * time.Second), Affected: 300}
	var data bytes.Buffer
	w := NewWriter(&data)
	w.Write(Message{Type: "stats", PID: 42, Stats: &Stats{Lateness: Lateness{Packets: 1}}})
	for i := range 300 {
		session := strconv.Itoa(i)
		e.Sessions = append(e.Sessions, session)
		report := &callharness.Report{StartedAt: at}
		if i < 256 {
			move := callharness.MoveReport{Kind: "takeover", From: 0, To: 1, Start: 5 * time.Second}
			move.Measurement.AudioLoss.Trusted = true
			report.Moves = []callharness.MoveReport{move}
		}
		w.Write(Message{Type: "call", PID: 42, CallID: i, Session: session, Call: report})
	}
	w.Write(Message{Type: "done", PID: 42})
	p, err := ReadProcess(&data, 42, 300, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	run := RunReport{Callers: 300, Processes: []ProcessReport{p}, Events: []EventResult{e}}
	run.Judge()
	if run.Outcome != "inconclusive" || run.Events[0].Recorded != 256 || len(run.Causes) != 1 || !strings.Contains(run.Causes[0], "256/300") {
		t.Fatalf("missing affected calls passed: outcome=%s events=%v causes=%v", run.Outcome, run.Events, run.Causes)
	}
	for _, row := range run.Yardstick {
		if row.Metric == "audio_loss" && (row.Events != 300 || row.TrustedCoverage != float64(256)/300) {
			t.Fatal(row)
		}
	}
}

func TestYardstickKeepsRepeatedMovesSeparate(t *testing.T) {
	at := time.Now()
	first := callharness.MoveReport{Kind: "move", To: 1, Start: 5 * time.Second}
	first.Measurement.AudioLoss.Trusted = true
	second := callharness.MoveReport{Kind: "move", From: 1, To: 2, Start: 10 * time.Second}
	run := RunReport{Processes: []ProcessReport{{Complete: true, Calls: []Message{{Session: "a", Call: &callharness.Report{StartedAt: at, Moves: []callharness.MoveReport{first, second}}}}}}, Events: []EventResult{
		{Event: Event{Kind: "move", Worker: 1, After: 5 * time.Second}, At: at.Add(5 * time.Second), FinishedAt: at.Add(6 * time.Second), Affected: 1, Sessions: []string{"a"}},
		{Event: Event{Kind: "move", Worker: 2, After: 10 * time.Second}, At: at.Add(10 * time.Second), FinishedAt: at.Add(11 * time.Second), Affected: 1, Sessions: []string{"a"}},
	}}
	run.Judge()
	var rows []MetricRow
	for _, row := range run.Yardstick {
		if row.Metric == "audio_loss" {
			rows = append(rows, row)
		}
	}
	if run.Outcome != "valid" || len(rows) != 2 || rows[0].Outcome != "pass" || rows[1].Outcome != "inconclusive" || rows[1].EventIndex != 1 {
		t.Fatalf("%s %v", run.Outcome, rows)
	}
	// A same-kind record from a different action cannot stand in for this event.
	run.Events[1].At = run.Events[1].At.Add(time.Second)
	run.Judge()
	if run.Outcome != "inconclusive" || run.Events[1].Recorded != 0 {
		t.Fatal("unrelated move counted as affected evidence")
	}
}

func TestSharedStatusPollerCoalescesSuccessesAndFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				if fail {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"calls": []any{}})
			}))
			defer server.Close()
			meter := &Meter{}
			poller := newStatusPoller(server.URL+"/calls", meter, nil)
			at := time.Now()
			poller.now = func() time.Time { return at }
			h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:20000"), Status: poller.Get}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = h.Close() }()
			for want := int64(1); want <= 2; want++ {
				var wg sync.WaitGroup
				for range 100 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := h.Status(context.Background())
						if (err != nil) != fail {
							t.Errorf("poll err=%v", err)
						}
					}()
				}
				wg.Wait()
				if requests.Load() != want {
					t.Fatalf("100 readers caused %d requests, want %d", requests.Load(), want)
				}
				at = at.Add(time.Second)
			}
			s := meter.sample()
			if s.requests != 2 || s.intervalRequests != 2 {
				t.Fatal("request telemetry missing", s.requests)
			}
		})
	}
}

func TestCallerStartDeadlineAndParentDeath(t *testing.T) {
	for _, future := range []bool{false, true} {
		start := make(chan time.Time, 1)
		if future {
			start <- time.Now().Add(time.Hour)
		}
		if err := waitForStart(context.Background(), start, 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "start deadline") {
			t.Fatalf("unbounded start: %v", err)
		}
	}
	var parent atomic.Int64
	parent.Store(int64(os.Getppid()))
	ctx, cancel := watchParent(context.Background(), int(parent.Load()), time.Millisecond, func() int { return int(parent.Load()) })
	defer cancel()
	parent.Store(1)
	if err := waitForStart(ctx, make(chan time.Time), time.Second); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("parent death did not cancel waiting child: %v", err)
	}
}
