package operatorcaller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

// Local is the unchanged child's loopback operator API.
type Local func(context.Context, string, any, any) error

func CollectProbes(ctx context.Context, count int, end time.Time, local Local, c Clock) (Burst, error) {
	end = end.Add(-AckReserve)
	if count < ClockBurstCount || count > 32 || end.IsZero() {
		return Burst{}, errors.New("invalid-probe-request")
	}
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	for c.Now().Before(end) {
		var raw []callharness.ClockProbe
		if err := local(ctx, "/clock-probes", nil, &raw); err != nil {
			return Burst{}, err
		}
		probes := make([]Probe, 0, count)
		for _, p := range raw[:min(count, len(raw))] {
			if observe.Numbers(p) != nil {
				return Burst{}, errors.New("invalid-clock-probe")
			}
			elapsed := p.CallerReceive.Sub(p.CallerSend)
			processing := p.ServerSend.Sub(p.ServerReceive)
			if processing < 0 || processing > elapsed {
				return Burst{}, errors.New("clock-probe-order")
			}
			probes = append(probes, Probe{int64(elapsed), p.CallerSend, p.CallerReceive, p.ServerReceive, p.ServerSend})
		}
		if !c.Now().Before(end) {
			break
		}
		if len(probes) >= count {
			b := Burst{probes[0].CallerSend, probes[len(probes)-1].CallerReceive, probes}
			return b, b.Validate()
		}
		if err := c.Sleep(ctx, min(time.Second, end.Sub(c.Now()))); err != nil {
			return Burst{}, err
		}
	}
	return Burst{}, errors.New("caller-probe-deadline")
}
func Deliver(ctx context.Context, record Feed, sessions []string, local Local, hangup func(), c Clock) (Ack, error) {
	ack := Ack{RunID: record.RunID, Job: "caller-1", Sequence: record.Sequence, OperationID: record.OperationID}
	p := record.Payload
	switch p.Kind {
	case "probe":
		b, err := CollectProbes(ctx, p.Count, p.Deadline, local, c)
		if err != nil {
			return ack, err
		}
		ack.ClockProbes, _ = json.Marshal(b)
	case "start":
		if err := local(ctx, "/start", loadgen.StartRequest{At: p.At}, nil); err != nil {
			return ack, err
		}
	case "arm":
		if !c.Now().Before(p.Deadline) {
			return ack, errors.New("expired-arm")
		}
		for _, id := range p.Identities {
			if !slices.Contains(sessions, id.SessionID) {
				continue
			}
			if id.RunID != record.RunID || id.OperationID != record.OperationID {
				return ack, errors.New("feed-identity")
			}
			if err := local(ctx, "/arm-event", id, nil); err != nil {
				return ack, err
			}
			ack.Identities = append(ack.Identities, id)
		}
		if err := local(ctx, "/clock-probes", nil, &ack.ClockProbes); err != nil {
			return ack, err
		}
	case "receipt":
		receipts := make([]Receipt, 0, len(p.Receipts))
		for _, r := range p.Receipts {
			if !slices.Contains(sessions, r.Identity.SessionID) {
				continue
			}
			if r.Identity.RunID != record.RunID || r.Identity.OperationID != record.OperationID {
				return ack, errors.New("receipt-identity")
			}
			receipts = append(receipts, r)
		}
		if len(receipts) > 0 {
			if err := local(ctx, "/record-events", receipts, nil); err != nil {
				return ack, err
			}
		}
	case "finish":
	case "hangup":
		hangup()
	default:
		return ack, errors.New("unknown-feed-kind")
	}
	ack.OK = true
	return ack, nil
}

// AwaitBegin is the production pre-spawn barrier. A terminal hangup is read
// separately, so a missing numbered feed can never hide shutdown.
func AwaitBegin(ctx context.Context, m Manifest, box Store, c Clock, setup Setup) error {
	authorized := setup.RuleAddedAt
	if authorized.IsZero() || authorized.After(c.Now().Add(5*time.Second)) {
		return errors.New("invalid-rule-time")
	}
	end := minTime(authorized.Add(15*time.Minute), time.UnixMilli(m.Deadline))
	if err := c.Sleep(ctx, max(0, authorized.Add(15*time.Second).Sub(c.Now()))); err != nil {
		return err
	}
	setup.SetupReadyAt = c.Now()
	if createJSON(ctx, box, m.Job()+"/setup-ready.json", setup) != Committed {
		return errors.New("setup-publication-unknown")
	}
	for c.Now().Before(end) {
		stopped, err := Stopped(ctx, box, m.Identity)
		if err != nil {
			return err
		}
		if stopped {
			return errors.New("conductor-stopped")
		}
		begin, err := box.Read(ctx, sequenceKey(m.Root(), -1))
		if err != nil {
			return err
		}
		terminal, err := box.Read(ctx, sequenceKey(m.Root(), 0))
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.Now().Before(end) {
			break
		}
		var t Feed
		if terminal.Found && decode(terminal.Data, &t) == nil && t.SchemaVersion == 1 && t.RunID == m.RunID && t.Sequence == 0 && t.Payload.Kind == "hangup" {
			return errors.New("conductor-stopped")
		}
		if begin.Found {
			var b Feed
			if decode(begin.Data, &b) != nil || b.SchemaVersion != 1 || b.RunID != m.RunID || b.Sequence != -1 || b.OperationID != "caller-begin" || b.Payload.Kind != "begin" || !c.Now().Before(b.Payload.Deadline) {
				return errors.New("invalid-begin")
			}
			return nil
		}
		if terminal.Found {
			return errors.New("feed-before-begin")
		}
		if err = c.Sleep(ctx, min(500*time.Millisecond, end.Sub(c.Now()))); err != nil {
			return err
		}
	}
	return errors.New("begin-deadline")
}
func Stopped(ctx context.Context, box Store, i Identity) (bool, error) {
	r, err := box.Read(ctx, i.Root()+"/hangup.json")
	if err != nil || !r.Found {
		return false, err
	}
	var stop struct {
		SchemaVersion int    `json:"schemaVersion"`
		RunID         string `json:"runId"`
	}
	if err = decode(r.Data, &stop); err != nil {
		return false, err
	}
	return stop.SchemaVersion == 1 && stop.RunID == i.RunID, nil
}

// SaturationWindow keeps the original early-abort wall-time policy. Judge-side
// exclusion windows and media budgets still decide whether evidence qualifies.
type SaturationWindow struct {
	PlannedSeconds         float64
	recent                 []flag
	streak, FlaggedSeconds float64
	Reason                 string
}
type flag struct {
	seconds float64
	flagged bool
}

func (w *SaturationWindow) Add(raw json.RawMessage) (bool, error) {
	var s loadgen.Stats
	if err := decode(raw, &s); err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if decode(raw, &fields) != nil {
		return false, errors.New("missing-generator-stats")
	}
	required := func(m map[string]json.RawMessage, keys ...string) bool {
		for _, k := range keys {
			v, ok := m[k]
			if !ok || string(v) == "null" {
				return false
			}
		}
		return true
	}
	var lateness, pacing map[string]json.RawMessage
	_ = decode(fields["interval_lateness"], &lateness)
	_ = decode(fields["interval_pacing"], &pacing)
	if !required(fields, "interval_ns", "cpu_percent_per_gomaxprocs") || !required(lateness, "p99_ns") || !required(pacing, "sent_frames", "skipped_frames") || w.PlannedSeconds <= 0 || !finite(w.PlannedSeconds) || !finite(s.CPUPerGOMAXPROCS) || s.Interval <= 0 {
		return false, errors.New("missing-generator-stats")
	}
	invalid := observe.Numbers(s) != nil || s.IntervalLateness.P99 < 0 || w.PlannedSeconds > float64(maxSafeInteger) || !finite(w.FlaggedSeconds) || w.FlaggedSeconds < 0 || w.FlaggedSeconds > float64(maxSafeInteger) || !finite(w.streak) || w.streak < 0 || w.streak > float64(maxSafeInteger)
	if _, ok := observe.AddCount(int64(s.IntervalPacing.SentFrames), int64(s.IntervalPacing.SkippedFrames)); !ok {
		invalid = true
	}
	if _, ok := observe.AddCount(int64(s.Pacing.SentFrames), int64(s.Pacing.SkippedFrames)); !ok {
		invalid = true
	}
	if invalid {
		if w.Reason == "" {
			w.Reason = "Invalid generator statistics"
		}
		return true, errors.New("invalid-generator-numbers")
	}
	flagged := s.CPUPerGOMAXPROCS > 85 || s.IntervalLateness.P99 > 20*time.Millisecond || s.IntervalPacing.SkippedPercent() > 1
	seconds := s.Interval.Seconds()
	reason := func(r string) {
		if w.Reason == "" {
			w.Reason = r
		}
	}
	if seconds >= 2 || s.IntervalLateness.P99 >= 500*time.Millisecond || s.IntervalPacing.SkippedPercent() >= 50 {
		reason("Generator saturation hard ceiling")
	}
	if flagged {
		if seconds > float64(maxSafeInteger)-w.FlaggedSeconds || seconds > float64(maxSafeInteger)-w.streak {
			if w.Reason == "" {
				w.Reason = "Invalid generator statistics"
			}
			return true, errors.New("generator-total-overflow")
		}
		w.FlaggedSeconds += seconds
		w.streak += seconds
	} else {
		w.streak = 0
	}
	w.recent = append(w.recent, flag{seconds, flagged})
	total := 0.0
	for _, f := range w.recent {
		total += f.seconds
	}
	excess := total - 30
	for excess > 0 && len(w.recent) > 0 {
		trim := min(excess, w.recent[0].seconds)
		w.recent[0].seconds -= trim
		excess -= trim
		if w.recent[0].seconds <= 0 {
			w.recent = w.recent[1:]
		}
	}
	recentFlagged := 0.0
	for _, f := range w.recent {
		if f.flagged {
			recentFlagged += f.seconds
		}
	}
	if w.streak >= 5 || recentFlagged >= 10 {
		reason("Generator saturation sustained wall-time")
	}
	if w.FlaggedSeconds > w.PlannedSeconds*0.01 {
		reason("Generator saturation exceeds planned 1% budget")
	}
	return w.Reason != "", nil
}
func RecordGeneratorSample(raw json.RawMessage, w *SaturationWindow, save func() error, stop func(string) error, abort func()) error {
	stopping, err := w.Add(raw)
	if stopping || err != nil {
		abort()
	}
	if err != nil {
		return err
	}
	if err = save(); err != nil {
		return err
	}
	if stopping {
		return stop(w.Reason)
	}
	return nil
}
