package operatorcaller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
)

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}
type SystemClock struct{}

func (SystemClock) Now() time.Time                                   { return time.Now() }
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error { return sleep(ctx, d) }

type Ready struct {
	SchemaVersion int             `json:"schemaVersion"`
	RunID         string          `json:"runId"`
	PublicSHA     string          `json:"publicSha"`
	Job           string          `json:"job"`
	CallerID      string          `json:"githubRun"`
	Attempt       string          `json:"attempt"`
	Sessions      []string        `json:"sessions"`
	PID           int             `json:"pid"`
	ReadyAt       time.Time       `json:"readyAt"`
	ClockProbes   json.RawMessage `json:"clockProbes"`
	Host          Host            `json:"host"`
}
type Setup struct {
	SchemaVersion int       `json:"schemaVersion"`
	RunID         string    `json:"runId"`
	PublicSHA     string    `json:"publicSha"`
	Job           string    `json:"job"`
	CallerID      string    `json:"githubRun"`
	Attempt       string    `json:"attempt"`
	RuleAddedAt   time.Time `json:"ruleAddedAt"`
	SetupReadyAt  time.Time `json:"setupReadyAt"`
	SignalRTTsNS  []int64   `json:"signalRttsNs,omitempty"`
}

// Broker is owned by the session event loop. It never retries a publication.
type Broker struct {
	Manifest                        Manifest
	Mailbox                         *Mailbox
	Clock                           Clock
	Fence                           Fence
	sequence                        int
	prepared, collected             bool
	setupRTTs                       []int64
	closed                          bool
	Registration                    *Registration
	CallerPID                       int
	FirstFaultAt                    *time.Time
	ReadyObservedAt, MediaStartedAt time.Time
	Events                          []Receipt
}

func NewBroker(m Manifest, box *Mailbox, c Clock, f Fence) (*Broker, error) {
	if box == nil || box.Store == nil {
		return nil, errors.New("missing-shared-mailbox")
	}
	if err := m.Validate(m.Identity, c.Now()); err != nil {
		return nil, err
	}
	return &Broker{Manifest: m, Mailbox: box, Clock: c, Fence: f}, nil
}
func (b *Broker) write(ctx context.Context, key string, v any) error {
	if b.Fence.Latched() {
		return errors.New("caller-fenced")
	}
	if createJSON(ctx, b.Mailbox, key, v) != Committed {
		b.Fence.Halt("command-result-unknown")
		return errors.New("command-result-unknown")
	}
	return nil
}
func (b *Broker) wait(ctx context.Context, key string, end time.Time, v any, valid func() bool) error {
	end = minTime(end, time.UnixMilli(b.Manifest.Deadline))
	for b.Clock.Now().Before(end) {
		if b.closed {
			return errors.New("caller-stopped")
		}
		r, err := b.Mailbox.Read(ctx, key)
		if err != nil {
			return err
		}
		if !b.Clock.Now().Before(end) {
			break
		}
		if r.Found {
			if decode(r.Data, v) != nil || !valid() {
				return errors.New("invalid-caller-receipt")
			}
			return nil
		}
		stop, err := b.Mailbox.Read(ctx, b.Manifest.Job()+"/stop.json")
		if err != nil {
			return err
		}
		if stop.Found {
			return errors.New("caller-stopped")
		}
		if err := b.Clock.Sleep(ctx, min(500*time.Millisecond, end.Sub(b.Clock.Now()))); err != nil {
			return err
		}
	}
	return errors.New("caller-deadline")
}
func (b *Broker) Prepare(ctx context.Context, token string, end time.Time) ([]int64, error) {
	if b.prepared {
		return append([]int64{}, b.setupRTTs...), nil
	}
	if !tokenPattern.MatchString(token) {
		return nil, errors.New("invalid-caller-token")
	}
	for _, o := range []struct {
		key   string
		value any
	}{{"caller-token.json", map[string]string{"bearerToken": token}}, {"transport.json", b.Manifest}} {
		if err := b.write(ctx, b.Manifest.Root()+"/"+o.key, o.value); err != nil {
			return nil, err
		}
	}
	var setup Setup
	err := b.wait(ctx, b.Manifest.Job()+"/setup-ready.json", end, &setup, func() bool {
		if setup.RunID != b.Manifest.RunID || setup.PublicSHA != b.Manifest.PublicSHA || setup.CallerID != b.Manifest.CallerID || setup.Attempt != b.Manifest.Attempt || setup.Job != "caller-1" || len(setup.SignalRTTsNS) > 32 {
			return false
		}
		for _, rtt := range setup.SignalRTTsNS {
			if rtt <= 0 || rtt > int64(time.Second) {
				return false
			}
		}
		return true
	})
	if err == nil {
		b.prepared = true
		b.setupRTTs = append([]int64{}, setup.SignalRTTsNS...)
	}
	return setup.SignalRTTsNS, err
}
func (b *Broker) Feed(ctx context.Context, operation string, p Payload, end time.Time) (Ack, error) {
	if ctx.Err() != nil || !b.Clock.Now().Before(minTime(end, time.UnixMilli(b.Manifest.Deadline))) {
		return Ack{}, errors.New("caller-deadline")
	}
	if !b.Clock.Now().Before(time.UnixMilli(b.Manifest.Deadline)) || !operationPattern.MatchString(operation) {
		return Ack{}, errors.New("invalid-feed")
	}
	if b.sequence < 0 || b.sequence >= maxSafeInteger {
		return Ack{}, errors.New("caller-sequence-exhausted")
	}
	n := b.sequence
	b.sequence++
	record := Feed{1, b.Manifest.RunID, n, operation, p}
	if err := b.write(ctx, sequenceKey(b.Manifest.Root(), n), record); err != nil {
		return Ack{}, err
	}
	var ack Ack
	err := b.wait(ctx, ackKey(b.Manifest.Job(), n), end, &ack, func() bool {
		return ack.RunID == record.RunID && ack.Job == "caller-1" && ack.Sequence == n && ack.OperationID == operation && ack.OK
	})
	if err != nil {
		b.Fence.Halt("command-result-unknown")
		return ack, errors.New("command-result-unknown")
	}
	if p.Kind == "receipt" {
		raw, _ := json.Marshal(p.Receipts)
		var copied []Receipt
		_ = decode(raw, &copied)
		b.Events = append(b.Events, copied...)
	}
	return ack, nil
}
func (b *Broker) Begin(ctx context.Context, end time.Time) (*Registration, error) {
	m := b.Manifest
	if err := b.write(ctx, sequenceKey(m.Root(), -1), Feed{1, m.RunID, -1, "caller-begin", Payload{Kind: "begin", Deadline: time.UnixMilli(m.Deadline)}}); err != nil {
		return nil, err
	}
	var ready Ready
	if err := b.wait(ctx, m.Job()+"/ready.json", end, &ready, func() bool {
		return ready.RunID == m.RunID && ready.PublicSHA == m.PublicSHA && ready.CallerID == m.CallerID && ready.Attempt == m.Attempt && ready.Job == "caller-1" && ready.PID > 0
	}); err != nil {
		return nil, err
	}
	b.ReadyObservedAt = b.Clock.Now()
	ack, err := b.Feed(ctx, "caller-probes", Payload{Kind: "probe", Count: ClockBurstCount, Deadline: time.UnixMilli(m.Deadline)}, end)
	if err != nil {
		return nil, err
	}
	var burst Burst
	if err = decode(ack.ClockProbes, &burst); err != nil {
		return nil, err
	}
	registration, err := Register(m.Plan, m.RunID, ready.Sessions, burst, ready.Host, b.Clock.Now(), nil)
	if err != nil {
		return nil, err
	}
	b.Registration = &registration
	b.CallerPID = ready.PID
	if err = b.write(ctx, m.Root()+"/registration.json", registration); err != nil {
		return nil, err
	}
	if !registration.Near {
		return &registration, errors.New("caller-not-near")
	}
	b.MediaStartedAt = b.Clock.Now().Add(time.Second)
	_, err = b.Feed(ctx, "caller-start", Payload{Kind: "start", At: b.MediaStartedAt}, end)
	return &registration, err
}
func (b *Broker) Arm(ctx context.Context, operation string, ids []callharness.OperationIdentity, end time.Time) error {
	if b.Registration == nil || !b.Registration.Near || len(ids) != len(b.Registration.Sessions) {
		return errors.New("unregistered-fault")
	}
	expected := map[string]bool{}
	for _, s := range b.Registration.Sessions {
		expected[s] = true
	}
	for _, id := range ids {
		if !id.Valid() || id.Epoch > maxSafeInteger || !expected[id.SessionID] || id.RunID != b.Manifest.RunID || id.OperationID != operation || !sessionPattern.MatchString(id.Worker) {
			return errors.New("fault-identity")
		}
		delete(expected, id.SessionID)
	}
	ack, err := b.Feed(ctx, operation, Payload{Kind: "arm", Identities: ids, Deadline: time.UnixMilli(b.Manifest.Deadline)}, end)
	if err != nil {
		return err
	}
	if !sameIdentities(ack.Identities, ids) {
		return errors.New("arm-ack-identity")
	}
	return nil
}
func sameIdentities(a, b []callharness.OperationIdentity) bool {
	if len(a) > maxSafeInteger || len(b) > maxSafeInteger || len(a) != len(b) {
		return false
	}
	counts := map[callharness.OperationIdentity]int{}
	for _, id := range a {
		counts[id]++
	}
	for _, id := range b {
		counts[id]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
func (b *Broker) MarkFault(ctx context.Context) error {
	if b.Registration == nil || !b.Registration.Near {
		return errors.New("unregistered-fault")
	}
	if b.Clock.Now().UnixMilli() == b.Registration.RegisteredAt {
		if err := b.Clock.Sleep(ctx, time.Millisecond); err != nil {
			return err
		}
	}
	at := b.Clock.Now()
	if at.UnixMilli() <= b.Registration.RegisteredAt || at.UnixMilli() >= b.Manifest.Deadline {
		return errors.New("fault-time")
	}
	if b.FirstFaultAt == nil {
		b.FirstFaultAt = &at
	}
	return nil
}
func (b *Broker) Hangup(ctx context.Context, reason string) bool {
	b.closed = true
	return Hangup(ctx, b.Mailbox, b.Manifest.Root(), b.Manifest.RunID, reason, b.Fence)
}
func Hangup(ctx context.Context, m *Mailbox, root, run, reason string, f Fence) bool {
	if !reasonPattern.MatchString(reason) {
		return false
	}
	state := m.hangup(ctx, root+"/hangup.json", map[string]any{"schemaVersion": 1, "runId": run, "reason": reason})
	// Only the conductor owns this key. Its create-only conflict proves an
	// earlier terminal hangup landed, never that arbitrary feed content matches.
	if state == Committed || state == Conflict {
		return true
	}
	f.Halt("command-result-unknown")
	return false
}

// EvidenceRetentionContext bounds one read-only retention step after collection
// ends. It cannot authorize a command, qualify cancelled collection, or retry it.
// Report reading and judgment each get at most thirty seconds.
func EvidenceRetentionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func (b *Broker) Collect(ctx context.Context, end time.Time) (string, error) {
	var finished struct {
		RunID    string   `json:"runId"`
		Job      string   `json:"job"`
		Done     bool     `json:"done"`
		Stopped  bool     `json:"stopped"`
		ExitCode *int     `json:"exitCode"`
		Sessions []string `json:"sessions"`
	}
	completionErr := b.wait(ctx, b.Manifest.Job()+"/finished.json", end, &finished, func() bool {
		return finished.RunID == b.Manifest.RunID && finished.Job == "caller-1" && finished.Done && !finished.Stopped && finished.ExitCode != nil && *finished.ExitCode == 0
	})
	b.closed = true
	if b.Registration == nil || !reflect.DeepEqual(finished.Sessions, b.Registration.Sessions) {
		completionErr = errors.Join(completionErr, errors.New("final-caller-sessions"))
	}
	// Read once even after unsuccessful completion. The report may contain
	// independent failures; completion certainty still controls boundary dialing.
	retained, cancel := EvidenceRetentionContext(ctx)
	defer cancel()
	raw, err := b.Mailbox.Read(retained, b.Manifest.Job()+"/caller.jsonl")
	completionErr = errors.Join(completionErr, ctx.Err())
	if !b.Clock.Now().Before(minTime(end, time.UnixMilli(b.Manifest.Deadline))) {
		completionErr = errors.Join(completionErr, errors.New("caller-deadline"))
	}
	if err != nil {
		return "", errors.Join(completionErr, err)
	}
	if !raw.Found || len(raw.Data) == 0 {
		return "", errors.Join(completionErr, errors.New("missing-raw-caller"))
	}
	b.collected = completionErr == nil
	return string(raw.Data), completionErr
}

// PrepareCallers replaces hosted candidate search with one launch per phase.
// For load, boundary readiness precedes survivor Begin. Boundary Begin itself
// belongs after survivor collection/stop, so their media can never overlap.
func PrepareCallers(ctx context.Context, l Launcher, survivor, boundary *Broker, token string, end time.Time, precondition func(context.Context) error) error {
	if err := precondition(ctx); err != nil {
		return err
	}
	if l.Ready(ctx, end) != "succeeded" {
		return errors.New("caller-not-ready")
	}
	for _, b := range []*Broker{survivor, boundary} {
		if b == nil {
			continue
		}
		r := l.Start(ctx, b.Manifest.Identity, b.Manifest.Phase, end)
		if r.State != Started {
			return errors.New(string(r.State))
		}
	}
	for _, b := range []*Broker{survivor, boundary} {
		if b == nil {
			continue
		}
		rtts, err := b.Prepare(ctx, token, end)
		if err != nil {
			return err
		}
		if len(rtts) > 0 {
			near := false
			for _, rtt := range rtts {
				near = near || rtt <= int64(NearRTT)
			}
			if !near {
				return errors.New("caller-not-near")
			}
		}
	}
	if survivor.Fence.Latched() {
		return errors.New("command-result-unknown")
	}
	_, err := survivor.Begin(ctx, end)
	return err
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{64}$`)
var reasonPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// BeginBoundary requires terminal survivor evidence and an observed inactive
// unit before the two-call phase can dial. Stop uncertainty is never erased.
func BeginBoundary(ctx context.Context, l Launcher, survivor, boundary *Broker, token string, end time.Time) error {
	if !survivor.collected || survivor.Registration == nil || !survivor.Registration.Near {
		return errors.New("survivor-not-terminal")
	}
	stop := l.Stop(ctx, survivor.Manifest.Identity, end)
	if !stop.Inactive || stop.Certainty != "succeeded" {
		survivor.Fence.Halt("command-result-unknown")
		return errors.New("command-result-unknown")
	}
	if !survivor.Clock.Now().Before(end) || survivor.Fence.Latched() {
		return errors.New("boundary-fenced")
	}
	rtts, err := boundary.Prepare(ctx, token, end)
	if err != nil {
		return err
	}
	if len(rtts) > 0 {
		near := false
		for _, rtt := range rtts {
			near = near || rtt <= int64(NearRTT)
		}
		if !near {
			return errors.New("caller-not-near")
		}
	}
	_, err = boundary.Begin(ctx, end)
	return err
}
