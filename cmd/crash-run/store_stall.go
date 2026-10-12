package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
)

type storeStallOptions struct {
	ResumedAt                               time.Time
	Redis                                   *clusterprocess.Child
	Variant                                 string
	Duration, Actual                        time.Duration
	store                                   *sessionstore.Redis
	controlURL                              string
	before                                  map[string]float64
	SessionID, Owner, WorkerURL, MediaError string
	TimedOut                                bool
	ObservationErrors                       []string
	Observations                            []storeStallObservation
}

type storeStallEvidence struct {
	StalledAt, ActionAt, ResumedAt, ObservedUntil time.Time
	SessionID, Owner                              string
	Variant                                       string                  `json:"variant"`
	RequestedStall                                time.Duration           `json:"requested_stall_ns"`
	ActualStall                                   time.Duration           `json:"actual_stall_ns"`
	Takeover                                      controlplane.MoveResult `json:"takeover"`
	RequiredRTP, RequiredSRTCP, DefaultRTPCap     uint64
	ExpectedOutcome, Outcome                      string
	EnvelopeEvents, LossEvents                    float64
	LeaseReleased, RoutesCleared                  bool
	Failures                                      []string
	TimedOut                                      bool
	MediaError                                    string
	Observations                                  []storeStallObservation
	MediaStopped                                  bool
	ConnectedThroughout                           bool
	LastConnectionState, LastICEState             string
	MediaSilence                                  map[string]time.Duration
	ControlCounters                               map[string]float64
	HangupError                                   string
}

func validateStoreStall(variant string, duration time.Duration, redis, topology string, incompatible bool) error {
	if variant == "" {
		if duration != 0 {
			return errors.New("store-stall-duration requires -store-stall")
		}
		return nil
	}
	if variant != "moderate" && variant != "long" && variant != "live" {
		return errors.New("store-stall must be moderate, long or live")
	}
	if duration < 0 {
		return errors.New("store-stall-duration must be positive")
	}
	if redis != "" {
		return errors.New("store-stall requires driver-owned throwaway Redis; unset RELAIS_REDIS_ADDR and omit -redis")
	}
	if topology != "loopback" || incompatible {
		return errors.New("store-stall requires loopback and cannot combine with other scenarios")
	}
	return nil
}

type storeStallObservation struct {
	At                          time.Time
	Status                      controlplane.Status
	RelayRoutes                 int
	WorkerSessions              int
	LeaseExists, SnapshotExists bool
	Errors                      []string
}

func (s *storeStallOptions) resume(stalledAt time.Time) error {
	if err := s.Redis.Signal(syscall.SIGCONT); err != nil {
		return err
	}
	s.ResumedAt = time.Now()
	s.Actual = s.ResumedAt.Sub(stalledAt)
	fmt.Printf("SIGCONT store pid=%d actual_stall=%s\n", s.Redis.PID(), s.Actual)
	return nil
}

// Sample before Hangup can delete the call. Remote relay status is read before
// store reads that can invoke the store's normal expiry-pruning scripts.
func (s *storeStallOptions) capture(ctx context.Context, status controlplane.Status, relayURL string) {
	o := storeStallObservation{At: time.Now(), Status: status, WorkerSessions: -1, RelayRoutes: -1}
	remote := &controlplane.RemoteRelay{URL: relayURL}
	r, err := remote.Status(ctx)
	if err != nil {
		o.Errors = append(o.Errors, "relay status: "+err.Error())
	} else {
		o.RelayRoutes = r.Stats.Flows
	}
	if s.Variant == "live" {
		w, err := (&controlplane.RemoteWorker{URL: s.WorkerURL}).Status(ctx)
		if err != nil {
			o.Errors = append(o.Errors, "worker status: "+err.Error())
		} else {
			o.WorkerSessions = w.Sessions
		}
	}
	_, err = s.store.Get(ctx, s.SessionID)
	o.LeaseExists = err == nil
	if err != nil && !errors.Is(err, sessionstore.ErrNotFound) {
		o.Errors = append(o.Errors, "lease read: "+err.Error())
	}
	_, err = s.store.GetState(ctx, s.SessionID)
	o.SnapshotExists = err == nil
	if err != nil && !errors.Is(err, sessionstore.ErrNotFound) {
		o.Errors = append(o.Errors, "snapshot read: "+err.Error())
	}
	s.Observations = append(s.Observations, o)
}

// Independently evaluate ADR 0002's formula rather than calling the product's
// policy function. The fixed default cap is an upper bound: retained advances
// and the adoption-deadline caller reserve can reduce it further.
func storeStallMargins(age time.Duration, info mediaworker.CheckpointState) (rtp, rtcp, cap uint64) {
	r, c := max(5000, info.RTPPacketRate), max(100, info.SRTCPPacketRate)
	rtp = uint64(math.Ceil(age.Seconds()*r*1.25)) + 65
	rtcp = uint64(math.Ceil(age.Seconds()*c*1.25)) + 17
	reserve := max(10000, math.Ceil(2*r)+64)
	if reserve < 32766 {
		cap = uint64(32766 - reserve)
	}
	return
}

func envelopeCounter(policy string) string {
	return `relais_checkpoint_envelope_events_total{policy="` + policy + `"}`
}

func (s *storeStallOptions) prepare(ctx context.Context, env []string, id, controlURL string) error {
	if s.Redis == nil {
		return errors.New("store-stall has no owned Redis child")
	}
	key, err := hex.DecodeString(envValue(env, "RELAIS_SESSIONSTORE_KEY"))
	if err != nil {
		return err
	}
	s.store, err = sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: envValue(env, "RELAIS_REDIS_ADDR"), Prefix: envValue(env, "RELAIS_REDIS_PREFIX")}, key)
	if err != nil {
		return err
	}
	state, err := s.store.GetState(ctx, id)
	if err != nil {
		s.close()
		return fmt.Errorf("pre-stall checkpoint: %w", err)
	}
	info, err := mediaworker.SnapshotCheckpoint(state)
	if err != nil || info.Successes == 0 {
		s.close()
		return fmt.Errorf("pre-stall checkpoint not established: successes=%d error=%v", info.Successes, err)
	}
	s.controlURL = controlURL
	s.before, _, err = scrapeMetrics(ctx, &http.Client{Timeout: time.Second}, controlURL+"/metrics")
	if err != nil {
		s.close()
	}
	return err
}
func (s *storeStallOptions) close() {
	if s.store != nil {
		_ = s.store.Close()
	}
}

// Pure judging seam: exercise rejected evidence without launching processes.
func judgeStoreStall(report *callharness.Report, status controlplane.Status, e *storeStallEvidence, relay string, stalledAt, killed, until time.Time) []string {
	var failures []string
	check := func(ok bool, why string) {
		if !ok {
			failures = append(failures, why)
		}
	}
	if report == nil {
		return []string{"missing caller report"}
	}
	if e.Takeover.LossCause == "ownership_expired" {
		return judgeOwnershipExpired(report, status, e, relay, stalledAt, killed)
	}
	if len(status.Takeovers) == 0 {
		e.Outcome = "missing-takeover"
		e.ExpectedOutcome = "resume"
		if e.Variant == "long" {
			e.ExpectedOutcome = "definitive-loss"
		}
		check(report.DecryptionFailures.Total() == 0, "caller decryption failures")
		check(report.ICERestarts == 0 && report.Renegotiations == 0, "caller reconnected or renegotiated")
		failures = append(failures, "no terminal takeover; expected "+e.ExpectedOutcome)
		return failures
	}
	t := e.Takeover
	e.RequiredRTP, e.RequiredSRTCP, e.DefaultRTPCap = storeStallMargins(t.SnapshotAge, t.Checkpoint)
	e.ExpectedOutcome = "resume"
	if max(e.RequiredRTP, 8192) > e.DefaultRTPCap || e.RequiredSRTCP > 1<<31-1 {
		e.ExpectedOutcome = "definitive-loss"
	}
	e.Outcome = "resume"
	if t.Lost {
		e.Outcome = "definitive-loss"
	}
	check(e.Outcome == e.ExpectedOutcome, "outcome differs from ADR formula")
	check((e.Variant == "moderate" && e.ExpectedOutcome == "resume") || (e.Variant == "long" && e.ExpectedOutcome == "definitive-loss"), "duration did not exercise selected variant")
	check(t.SnapshotAge > 550*time.Millisecond && t.SnapshotAge >= t.CheckpointAge && t.CheckpointAge > 0 && !t.CheckpointStoredAt.IsZero() && t.Checkpoint.Successes > 0, "missing or invalid checkpoint age evidence")
	check(e.EnvelopeEvents == 1, "expected exactly one envelope event")
	check(report.DecryptionFailures.Total() == 0, "caller decryption failures")
	check(report.ICERestarts == 0 && report.Renegotiations == 0 && report.OfferAnswerExchanges == 1 && report.RemoteAddr == relay, "caller reconnected, renegotiated or changed address")
	check(len(report.Moves) == 1 && len(status.Takeovers) == 1, "expected exactly one takeover")
	check(len(report.Tracks) == 2, "missing caller audio/video")
	for _, track := range report.Tracks {
		pastEnvelope := false
		for _, at := range track.Arrivals {
			arrival := report.StartedAt.Add(at)
			if arrival.After(stalledAt.Add(550*time.Millisecond)) && arrival.Before(killed) {
				pastEnvelope = true
			}
		}
		check(pastEnvelope, "no "+track.Kind+" media flowing past envelope while Redis stopped")
		if t.Lost {
			check(!report.StartedAt.Add(track.LastArrival).After(t.End.Add(50*time.Millisecond)), "media resumed after definitive loss")
		} else {
			check(report.StartedAt.Add(track.LastArrival).After(t.End) && until.Sub(report.StartedAt.Add(track.LastArrival)) < 200*time.Millisecond, "no continuing "+track.Kind+" media after resume")
		}
	}
	if t.Lost {
		check(t.CheckpointPolicy == "definitive-loss" && len(status.Calls) == 0 && status.LostCount == 1 && e.LossEvents == 1, "definitive loss not counted or call retained")
		check(t.SequenceMargin == 0 && t.SRTCPIndexMargin == 0, "lost snapshot spent resume margins")
		check(e.LeaseReleased && e.RoutesCleared, "loss retained lease or persisted relay route")
	} else {
		check(t.CheckpointPolicy == "scaled" && t.Error == "" && len(status.Calls) == 1 && status.Calls[0].Owner != t.From && status.LostCount == 0 && e.LossEvents == 0, "scaled resume missing or call lost")
		check(uint64(t.SequenceMargin) == max(8192, e.RequiredRTP) && uint64(t.SRTCPIndexMargin) == max(128, e.RequiredSRTCP), "resume margins differ from ADR formula")
		check(report.ConnectedThroughout(), "caller connection interrupted during resumed call")
	}
	return failures
}

func (s *storeStallOptions) judge(ctx context.Context, report *callharness.Report, hangupErr error, status controlplane.Status, ready clusterprocess.Ready, stalledAt, killed, until time.Time, dir string) (result, error) {
	e := &storeStallEvidence{Variant: s.Variant, RequestedStall: s.Duration, ActualStall: s.Actual, TimedOut: s.TimedOut, MediaError: s.MediaError, Observations: s.Observations, StalledAt: stalledAt, ActionAt: killed, ResumedAt: s.ResumedAt, ObservedUntil: until, SessionID: s.SessionID, Owner: s.Owner}
	takeovers := crashTakeovers(status.Takeovers)
	if len(takeovers) > 0 {
		e.Takeover = takeovers[len(takeovers)-1]
	}
	values, _, err := scrapeMetrics(ctx, &http.Client{Timeout: time.Second}, s.controlURL+"/metrics")
	if err != nil {
		return result{}, err
	}
	for _, policy := range []string{"scaled", "definitive-loss"} {
		name := envelopeCounter(policy)
		before, bOK := s.before[name]
		after, aOK := values[name]
		if !bOK || !aOK {
			return result{}, fmt.Errorf("missing envelope counter %s", name)
		}
		delta := after - before
		e.EnvelopeEvents += delta
		if policy == "definitive-loss" {
			e.LossEvents = delta
		}
	}
	if e.Takeover.Lost {
		_, err := s.store.Get(ctx, e.Takeover.ID)
		e.LeaseReleased = errors.Is(err, sessionstore.ErrNotFound)
		routes, err := s.store.LoadRoutes(ctx, 100)
		if err != nil {
			return result{}, err
		}
		e.RoutesCleared = true
		for _, route := range routes {
			if route.SessionID == e.Takeover.ID {
				e.RoutesCleared = false
			}
		}
	}
	if report == nil {
		return result{}, errors.New("missing caller report")
	}
	e.MediaSilence = map[string]time.Duration{}
	for _, track := range report.Tracks {
		gap, _, _ := relayRestartGap(track.Arrivals, stalledAt.Sub(report.StartedAt), until.Sub(report.StartedAt))
		e.MediaSilence[track.Kind] = gap
		if until.Sub(report.StartedAt.Add(track.LastArrival)) > 200*time.Millisecond {
			e.MediaStopped = true
		}
	}
	e.ConnectedThroughout = report.ConnectedThroughout()
	if len(report.ConnectionStates) > 0 {
		e.LastConnectionState = report.ConnectionStates[len(report.ConnectionStates)-1].State
	}
	if len(report.ICEConnectionStates) > 0 {
		e.LastICEState = report.ICEConnectionStates[len(report.ICEConnectionStates)-1].State
	}
	e.ControlCounters = map[string]float64{}
	for _, name := range []string{"relais_control_losses_total", "relais_control_takeovers_total", "relais_control_detection_events_total"} {
		if name == "relais_control_losses_total" {
			for _, cause := range []string{"ownership_expired", "checkpoint_unsafe", "checkpoint_missing", "recovery_failed"} {
				key := lossCounter(cause)
				before, bOK := s.before[key]
				after, aOK := values[key]
				if !bOK || !aOK {
					return result{}, fmt.Errorf("missing loss counter %s", key)
				}
				e.ControlCounters[key] = after - before
				e.ControlCounters[name] += after - before
			}
		} else {
			e.ControlCounters[name] = values[name] - s.before[name]
		}
	}
	e.LossEvents = e.ControlCounters["relais_control_losses_total"]
	if s.Variant == "live" {
		e.ExpectedOutcome = "observation"
		e.Outcome = "media-flowing"
		if e.MediaStopped {
			e.Outcome = "media-stopped"
		}
		e.Failures = judgeOwnershipExpired(report, status, e, ready.Media, stalledAt, killed)
	} else {
		e.Failures = judgeStoreStall(report, status, e, ready.Media, stalledAt, killed, until)
	}
	e.Failures = append(e.Failures, s.ObservationErrors...)
	for _, o := range e.Observations {
		e.Failures = append(e.Failures, o.Errors...)
	}
	if hangupErr != nil {
		e.HangupError = hangupErr.Error()
	}
	if hangupErr != nil && !((e.Takeover.Lost || len(status.Calls) == 0) && strings.Contains(hangupErr.Error(), "404")) {
		e.Failures = append(e.Failures, "hangup: "+hangupErr.Error())
	}
	r := result{StoreStall: e, Pass: len(e.Failures) == 0, Decrypt: report.DecryptionFailures.Total(), Reconnects: report.ICERestarts, Renegotiations: report.Renegotiations}
	for _, gap := range e.MediaSilence {
		r.Gap = max(r.Gap, gap)
	}
	for name, v := range map[string]any{"caller-report.json": report, "store-stall.json": r} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return r, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0600); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Live trials judge evidence completeness and caller integrity, not survival.
// A fenced, silent call is an observed product behaviour, not acceptance.
func judgeLiveStoreStall(report *callharness.Report, e *storeStallEvidence, stalledAt, resumedAt time.Time) []string {
	var failures []string
	if report == nil {
		return []string{"missing caller report"}
	}
	if report.DecryptionFailures.Total() != 0 {
		failures = append(failures, "caller decryption failures")
	}
	if report.ICERestarts != 0 || report.Renegotiations != 0 || report.OfferAnswerExchanges != 1 {
		failures = append(failures, "caller reconnected or renegotiated")
	}
	if len(report.Tracks) != 2 || len(e.Observations) == 0 {
		failures = append(failures, "missing media or process observations")
	}
	for _, track := range report.Tracks {
		flowing := false
		for _, at := range track.Arrivals {
			t := report.StartedAt.Add(at)
			if t.After(stalledAt.Add(550*time.Millisecond)) && t.Before(resumedAt) {
				flowing = true
			}
		}
		if !flowing {
			failures = append(failures, "no "+track.Kind+" media past envelope during stall")
		}
	}
	return failures
}

func runStoreStallTrials(ctx context.Context, manager *clusterprocess.Manager, redis *clusterprocess.Child, bin, dir, key, addr, variant string, duration time.Duration, runs int, after time.Duration, verbose bool) error {
	if duration == 0 {
		duration = 1500 * time.Millisecond
		if variant == "long" || variant == "live" {
			duration = 5 * time.Second
		}
	}
	for i := 0; i < runs; i++ {
		trialDir := filepath.Join(dir, fmt.Sprintf("store-stall-%s-%02d", variant, i+1))
		if err := os.Mkdir(trialDir, 0700); err != nil {
			return err
		}
		options := &storeStallOptions{Redis: redis, Variant: variant, Duration: duration}
		env := processEnv(key, addr, fmt.Sprintf("relais:crash:%s:stall:%d:", filepath.Base(dir), i))
		trialCtx, stop := context.WithTimeout(ctx, after+duration+30*time.Second)
		r, err := trial(trialCtx, manager, bin, trialDir, env, after, 1550*time.Millisecond+time.Duration(i)*7*time.Millisecond, verbose, false, false, nil, nil, options)
		stop()
		if err != nil {
			return trialFailure(i+1, "store-stall-"+variant, trialDir, err)
		}
		e := r.StoreStall
		fmt.Printf("STORE_STALL run=%d variant=%s stall=%s snapshot_age=%s checkpoint_age=%s required_rtp=%d cap=%d rtp_margin=%d srtcp_margin=%d outcome=%s losses=%g cause=%s decrypt=%d reconnects=%d renegotiations=%d envelope_events=%g pass=%t failures=%q\n", i+1, variant, e.ActualStall, e.Takeover.SnapshotAge, e.Takeover.CheckpointAge, e.RequiredRTP, e.DefaultRTPCap, e.Takeover.SequenceMargin, e.Takeover.SRTCPIndexMargin, e.Outcome, e.LossEvents, e.Takeover.LossCause, r.Decrypt, r.Reconnects, r.Renegotiations, e.EnvelopeEvents, r.Pass, e.Failures)
		if e.Takeover.LossCause == "ownership_expired" || len(e.Observations) > 0 && len(e.Observations[len(e.Observations)-1].Status.Takeovers) == 0 {
			fmt.Println("CHECKPOINT_AT_TAKEOVER NOT-RUN: no retained checkpoint age; ownership expiry is judged through terminal accounting")
		}
		fmt.Printf("STORE_STALL_CALLER run=%d gap=%s stopped=%t connection=%s ice=%s connected_throughout=%t timeout=%t observations=%+v\n", i+1, r.Gap, e.MediaStopped, e.LastConnectionState, e.LastICEState, e.ConnectedThroughout, e.TimedOut, e.Observations)
		if !r.Pass {
			return trialFailure(i+1, "store-stall-"+variant, trialDir, errors.New("store-stall acceptance failed; see store-stall.json and caller-report.json"))
		}
	}
	if variant == "live" {
		fmt.Printf("PASS: %d/%d live store-stall trials; expired ownership counted and cleaned\n", runs, runs)
	} else {
		fmt.Printf("PASS: %d/%d real-process store-stall trials variant=%s\n", runs, runs, variant)
	}
	return nil
}

func lossCounter(cause string) string { return `relais_control_losses_total{cause="` + cause + `"}` }

// Ownership is absent, so no checkpoint age/margins can be inferred. Judge
// terminal accounting and integrity directly instead of inventing an age.
func judgeOwnershipExpired(report *callharness.Report, status controlplane.Status, e *storeStallEvidence, relay string, stalledAt, actionAt time.Time) []string {
	e.ExpectedOutcome, e.Outcome = "definitive-loss", "definitive-loss"
	failures := judgeLiveStoreStall(report, e, stalledAt, actionAt)
	check := func(ok bool, why string) {
		if !ok {
			failures = append(failures, why)
		}
	}
	t := e.Takeover
	check((e.Variant == "long" || e.Variant == "live") && e.ActualStall > 3*time.Second, "duration did not expire ownership")
	check(t.Lost && t.LossCause == "ownership_expired" && len(status.Takeovers) == 1 && len(status.Calls) == 0 && status.LostCount == 1, "ownership loss missing, duplicated or call retained")
	check(e.LossEvents == 1 && e.ControlCounters[lossCounter("ownership_expired")] == 1, "ownership loss metric missing or duplicated")
	check(e.EnvelopeEvents == 0, "absent checkpoint falsely counted as envelope decision")
	check(t.SequenceMargin == 0 && t.SRTCPIndexMargin == 0 && t.SnapshotAge == 0, "expired ownership spent margins or invented checkpoint age")
	check(e.LeaseReleased && e.RoutesCleared, "ownership loss retained lease or persisted relay route")
	if len(e.Observations) > 0 {
		latest := e.Observations[len(e.Observations)-1]
		check(latest.RelayRoutes == 0, "ownership loss retained cached relay route")
		check(!latest.LeaseExists && !latest.SnapshotExists, "ownership loss retained lease or snapshot")
		if e.Variant == "live" {
			check(latest.WorkerSessions == 0 && e.ControlCounters["relais_control_detection_events_total"] == 0, "live worker fence was missing or misreported as worker death")
		}
	}
	if report != nil {
		check(report.RemoteAddr == relay && len(report.Moves) == 1, "missing terminal caller event or changed address")
		if len(report.Moves) == 1 {
			check(report.Moves[0].Lost && report.Moves[0].LossCause == "ownership_expired", "caller report missing loss cause")
		}
		for _, track := range report.Tracks {
			check(!report.StartedAt.Add(track.LastArrival).After(t.End.Add(50*time.Millisecond)), "media resumed after definitive loss")
		}
	}
	return failures
}
