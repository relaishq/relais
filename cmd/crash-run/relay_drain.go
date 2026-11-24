//go:build !windows

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
)

type relayDrainSample struct {
	Kind       string
	Gap, Limit time.Duration
}

func runRelayDrainTrials(ctx context.Context, manager *clusterprocess.Manager, bin, dir, key, addr string, runs int, after time.Duration) error {
	var samples []relayDrainSample
	for i := range runs {
		trialDir := filepath.Join(dir, fmt.Sprintf("relay-drain-%02d", i+1))
		if err := os.Mkdir(trialDir, 0700); err != nil {
			return err
		}
		bounded, cancel := context.WithTimeout(ctx, after+35*time.Second)
		err := relayContinuityTrial(bounded, manager, bin, trialDir, key, addr, after, false, true, &samples, nil, "", relayLossOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("relay drain trial %d: %w (logs %s)", i+1, err, trialDir)
		}
	}
	if err := judgeRelayDrainSamples(samples); err != nil {
		return err
	}
	fmt.Printf("PASS: %d shipped-process relay drains; 8 calls per drain; auth=0 duplicates=0 reconnects=0\n", runs)
	return nil
}

// ADR 0016 row 1 pools changes per track. In each full 20 changes at most
// one may exceed min(same-run baseline p95 + 50 ms, 150 ms). A smoke with
// fewer than 20 changes has no outlier allowance.
func judgeRelayDrainSamples(samples []relayDrainSample) error {
	for _, kind := range []string{"audio", "video"} {
		var gaps []time.Duration
		bad := 0
		for _, sample := range samples {
			if sample.Kind != kind {
				continue
			}
			if sample.Gap <= 0 || sample.Limit <= 0 {
				return fmt.Errorf("relay drain %s: missing timing evidence", kind)
			}
			gaps = append(gaps, sample.Gap)
			if sample.Gap > sample.Limit {
				bad++
			}
		}
		if len(gaps) == 0 {
			return fmt.Errorf("relay drain %s: no samples", kind)
		}
		fmt.Printf("RELAY_DRAIN track=%s n=%d p95=%s max=%s above_target=%d allowed=%d\n", kind, len(gaps), relayDrainPercentile(gaps, 95), slices.Max(gaps), bad, len(gaps)/20)
		if bad > len(gaps)/20 {
			return fmt.Errorf("relay drain %s: %d/%d gaps above planned-change target", kind, bad, len(gaps))
		}
	}
	return nil
}

func relayDrainPercentile(values []time.Duration, percentile int) time.Duration {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	return ordered[(len(ordered)*percentile+99)/100-1]
}

func relayDrainCalls(ctx context.Context, controlURL string, workerURLs []string, store *sessionstore.Redis, h *callharness.Harness, calls []*callharness.Call, after time.Duration) ([]relayDrainSample, error) {
	// Collect baseline intervals while all callers are already sending media.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
	}
	baselines := make(map[string]*callharness.Report, len(calls))
	for _, call := range calls {
		baselines[call.SessionID()] = call.RelaySnapshot()
	}
	started := time.Now()
	var result controlplane.RelayDrainResult
	if err := privateapi.Do(ctx, http.DefaultClient, controlURL, http.MethodPost, "/relays/r1/drain", nil, &result, nil); err != nil {
		return nil, err
	}
	if result.NotMovable != 0 || len(result.Moves) != len(calls) {
		return nil, fmt.Errorf("incomplete relay drain: %+v", result)
	}
	moved := make(map[string]bool, len(result.Moves))
	for _, move := range result.Moves {
		if moved[move.ID] || baselines[move.ID] == nil || move.From != "r1" || move.To != "r2" || move.Error != "" || move.NotMovable {
			return nil, fmt.Errorf("relay drain call outcome: %+v", move)
		}
		moved[move.ID] = true
	}
	confirmedAt := time.Now()
	if err := lossWait(ctx, func() bool {
		for _, call := range calls {
			if call.RelayRecoveryGap(started, confirmedAt) == 0 {
				return false
			}
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("decoded media after relay drain: %w", err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Second):
	}
	var samples []relayDrainSample
	for _, call := range calls {
		report := call.RelaySnapshot()
		baseline := baselines[call.SessionID()]
		for _, kind := range []string{"audio", "video"} {
			base, track := baseline.Track(kind), report.Track(kind)
			if base == nil || track == nil || len(base.Arrivals) < 20 {
				return nil, fmt.Errorf("call %s: missing %s baseline", call.SessionID(), kind)
			}
			var intervals []time.Duration
			for i := 1; i < len(base.Arrivals); i++ {
				intervals = append(intervals, base.Arrivals[i]-base.Arrivals[i-1])
			}
			var gap time.Duration
			for i := 1; i < len(track.Arrivals); i++ {
				if report.StartedAt.Add(track.Arrivals[i]).After(started) {
					gap = max(gap, track.Arrivals[i]-track.Arrivals[i-1])
				}
			}
			limit := min(150*time.Millisecond, relayDrainPercentile(intervals, 95)+50*time.Millisecond)
			samples = append(samples, relayDrainSample{Kind: kind, Gap: gap, Limit: limit})
			fmt.Printf("RELAY_DRAIN_SAMPLE call=%s track=%s gap=%s limit=%s\n", call.SessionID(), kind, gap, limit)
		}
	}
	// Keep consent and media live after the change. Timing above covers the
	// drain and settling phase; this observation separately checks survival.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(after):
	}
	var status controlplane.Status
	if err := privateapi.Do(ctx, http.DefaultClient, controlURL, http.MethodGet, "/status", nil, &status, nil); err != nil {
		return nil, err
	}
	if len(status.Calls) != len(calls) || status.LostCount != 0 {
		return nil, fmt.Errorf("relay drain lost calls: live=%d lost=%d", len(status.Calls), status.LostCount)
	}
	if !slices.Contains(status.Relays, controlplane.RelayStatus{Name: "r1", Draining: true}) {
		return nil, fmt.Errorf("drained relay is not empty: %+v", status.Relays)
	}
	for _, endpoint := range workerURLs {
		values, _, err := scrapeMetrics(ctx, http.DefaultClient, endpoint+"/metrics")
		if err != nil {
			return nil, err
		}
		if failures, ok := values["relais_worker_decryption_failures_total"]; !ok || failures != 0 {
			return nil, fmt.Errorf("worker decryption evidence missing or nonzero: present=%t failures=%g", ok, failures)
		}
	}
	for _, call := range calls {
		assignment, confirmation, err := store.RelayConfirmation(ctx, call.SessionID())
		if err != nil {
			return nil, err
		}
		if assignment != (sessionstore.RelayAssignment{Slot: "r2", Generation: 1}) || !confirmation.Confirmed(assignment) || confirmation.Episode.Open() {
			return nil, fmt.Errorf("call %s: drain not confirmed: %+v", call.SessionID(), assignment)
		}
		report := call.RelaySnapshot()
		if call.RelayRecoveryGap(started, started.Add(after)) == 0 {
			return nil, fmt.Errorf("call %s: decoded media did not survive observation", call.SessionID())
		}
		if err := judgeRelayDrainCall(report, call.RelayDTLSStates(), started.Add(after)); err != nil {
			return nil, fmt.Errorf("call %s: %w", call.SessionID(), err)
		}
	}
	call, err := h.Dial(ctx, callharness.CallOptions{RelayChange: true})
	if err != nil {
		return nil, err
	}
	_, assignment, err := store.RelayOwner(ctx, call.SessionID())
	if err != nil {
		return nil, err
	}
	if assignment.Slot != "r2" {
		return nil, fmt.Errorf("new call placed on drained relay: %+v", assignment)
	}
	_, err = call.Hangup(ctx)
	return samples, err
}

func judgeRelayDrainCall(report *callharness.Report, dtls []callharness.StateChange, liveSince time.Time) error {
	for _, kind := range []string{"audio", "video"} {
		track := report.Track(kind)
		if track == nil || !report.StartedAt.Add(track.LastArrival).After(liveSince) {
			return fmt.Errorf("no live %s media", kind)
		}
	}
	if report.DecryptionFailures.Total() != 0 {
		return fmt.Errorf("caller decryption failure")
	}
	for _, track := range report.Tracks {
		if track.DuplicatePackets != 0 {
			return fmt.Errorf("duplicate caller RTP")
		}
	}
	connected := 0
	for _, state := range dtls {
		if state.State == "connected" {
			connected++
		}
		if state.State == "failed" || state.State == "disconnected" || state.State == "closed" {
			return fmt.Errorf("DTLS connection interrupted")
		}
	}
	if connected != 1 || report.OfferAnswerExchanges != 2 || report.ICERestarts != 1 {
		return fmt.Errorf("unexpected reconnect or restart count: connected=%d exchanges=%d restarts=%d", connected, report.OfferAnswerExchanges, report.ICERestarts)
	}
	return nil
}
