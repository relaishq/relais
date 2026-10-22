package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// These helpers consume only the same scheduling interface as Run.
func recoveryWork(p *Plane, source *registration) []sessionstore.Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recovery.workLocked(source, recoveryAll)
}
func pendingRecovery(p *Plane, source *registration, id string) *sessionstore.Lease {
	for _, lease := range recoveryWork(p, source) {
		if lease.SessionID == id {
			return &lease
		}
	}
	return nil
}

// Obtain a retained checkpoint recovery through the ordinary move sequence.
func queueUncertainExport(t *testing.T, p *Plane, id string) {
	t.Helper()
	original := p.workers["a"].worker
	base := p.store
	p.workers["a"].worker = &uncertainExportWorker{fakeWorker: original.(*fakeWorker)}
	p.store = &recoveryErrorStore{Store: base, step: "transfer", fail: true}
	_, err := p.Move(context.Background(), id, "b")
	require.Error(t, err)
	p.store, p.workers["a"].worker = base, original
	require.NotNil(t, pendingRecovery(p, p.workers["a"], id))
}

type exportErrorWorker struct {
	*fakeWorker
	err error
}

func (w *exportErrorWorker) ExportSession(string) ([]byte, error) { return nil, w.err }
func requireExportOutcome(t *testing.T, cause error, uncertain bool) {
	t.Helper()
	p, a, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	before, err := p.store.Get(context.Background(), id)
	require.NoError(t, err)
	p.workers["a"].worker = &exportErrorWorker{fakeWorker: a, err: cause}
	p.store = &recoveryErrorStore{Store: p.store, step: "transfer", fail: true}
	result, err := p.Move(context.Background(), id, "b")
	require.Error(t, err)
	require.False(t, result.Lost)
	require.Equal(t, uncertain, pendingRecovery(p, p.workers["a"], id) != nil, "only uncertain export retains recovery")
	after, err := p.store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, before.Epoch, after.Epoch)
}

func settleRecovery(t *testing.T, p *Plane, source *registration, id string) {
	t.Helper()
	p.config.DeadAfter = time.Minute
	p.config.CheckInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	require.Eventually(t, func() bool { return pendingRecovery(p, source, id) == nil }, 3*time.Second, time.Millisecond, "detector must finish retained recovery")
}

type moveSequence struct {
	mu           sync.Mutex
	events       []string
	earlyRelease bool
}

func (s *moveSequence) note(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

type sequenceWorker struct {
	Worker
	sequence *moveSequence
	role     string
}

func (w *sequenceWorker) ExportSession(id string) ([]byte, error) {
	w.sequence.note(w.role + "-export")
	return w.Worker.ExportSession(id)
}
func (w *sequenceWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.sequence.note(w.role + "-resume")
	return w.Worker.ResumeSession(state, opts)
}

type sequenceRelay struct {
	*fakeRelay
	sequence  *moveSequence
	recipient *fakeWorker
}

func (r *sequenceRelay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	r.sequence.note("hold")
	return r.fakeRelay.HoldSession(ctx, id, from)
}
func (r *sequenceRelay) MoveSession(id string, from, to netip.AddrPort) error {
	r.sequence.note("route")
	return r.fakeRelay.MoveSession(id, from, to)
}
func (r *sequenceRelay) MoveSessionAtEpoch(id string, from, to netip.AddrPort, _ uint64) error {
	return r.MoveSession(id, from, to)
}
func (r *sequenceRelay) MoveSessionAfterExport(id string, from, to netip.AddrPort, _ uint64) error {
	r.sequence.note("output-drain")
	return r.MoveSession(id, from, to)
}
func (r *sequenceRelay) ReleaseSession(id string, to netip.AddrPort) (int, error) {
	r.sequence.mu.Lock()
	r.sequence.earlyRelease = r.sequence.earlyRelease || !r.recipient.runs(id)
	r.sequence.mu.Unlock()
	r.sequence.note("release")
	return 0, nil
}
func (r *sequenceRelay) ReleaseAfterExport(id string, to netip.AddrPort, _ map[uint32]relay.ConsumedInput) (relay.ReleaseReply, error) {
	_, err := r.ReleaseSession(id, to)
	return relay.ReleaseReply{}, err
}

func TestMoveRecoveryExactExportAndSequence(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprint(retained), func(t *testing.T) {
			p, a, b, r := setup(t)
			sequence := &moveSequence{}
			target := &observedResumeWorker{fakeWorker: b}
			p.workers["a"].worker = &sequenceWorker{Worker: &exportingWorker{fakeWorker: a}, sequence: sequence, role: "source"}
			p.workers["b"].worker = &sequenceWorker{Worker: target, sequence: sequence, role: "target"}
			p.relay = &sequenceRelay{fakeRelay: r, sequence: sequence, recipient: b}
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)
			if retained {
				p.store = &retainedMoveStore{Store: p.store}
			}
			_, err = p.Move(context.Background(), id, "b")
			if retained {
				require.Error(t, err)
				settleRecovery(t, p, p.workers["a"], id)
			} else {
				require.NoError(t, err)
			}
			require.True(t, b.runs(id))
			require.Zero(t, target.opts.SequenceMargin, "exact export must use zero RTP margin")
			require.Zero(t, target.opts.SRTCPIndexMargin, "exact export must use zero SRTCP margin")
			sequence.mu.Lock()
			defer sequence.mu.Unlock()
			require.False(t, sequence.earlyRelease, "hold released before its recipient ran")
			require.Equal(t, []string{"hold", "source-export", "output-drain", "route", "target-resume", "release"}, sequence.events, "output drain must precede route movement")
		})
	}
}

func TestMoveRecoveryRebuildConfirmedInstallation(t *testing.T) {
	p, a, _, r := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	authorizeTestControl(t, p)
	sequence := &moveSequence{}
	p.workers["a"].worker = &sequenceWorker{Worker: a, sequence: sequence, role: "source"}
	routing := &sequenceRelay{fakeRelay: r, sequence: sequence, recipient: a}
	p.SetRelay(routing)
	intent := sessionstore.ControlIntent{ID: id, Token: "interrupted", Kind: "move", Lease: lease, Started: time.Now()}
	require.NoError(t, p.restart.store.PutControlIntent(ctx, intent))
	installed := mediaworker.InstalledSession{ID: id, Owner: lease.Worker, Epoch: lease.Epoch}
	require.NoError(t, p.recovery.rebuild(ctx, intent, map[string]mediaworker.InstalledSession{id + "/" + fmt.Sprint(lease.Epoch) + "/" + lease.Worker.String(): installed}))
	require.True(t, a.runs(id), "confirmed installation must stay running")
	require.Equal(t, []string{"route", "release"}, sequence.events, "rebuild must repair routing without export or resume")
	intents, err := p.restart.store.ControlIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, intents)
	current, err := p.store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, lease.Epoch, current.Epoch)
}

type confirmationWorker struct {
	*observedResumeWorker
	requests []mediaworker.ResumeOptions
	states   [][]byte
}

func (w *confirmationWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.requests = append(w.requests, opts)
	w.states = append(w.states, append([]byte(nil), state...))
	if len(w.requests) == 1 {
		return "", privateapi.Uncertain(errors.New("first reply lost"))
	}
	return w.observedResumeWorker.ResumeSession(state, opts)
}
func (w *confirmationWorker) RequestKeyframe(context.Context, string) error { return nil }
func TestMoveRecoveryConfirmationKeepsAttempt(t *testing.T) {
	p, target, lease := checkpointRecovery(t, 150*time.Millisecond)
	ctx := context.Background()
	source := p.workers["a"]
	worker := &confirmationWorker{observedResumeWorker: target}
	p.workers["b"].worker = worker
	routing := &checkpointReplayRelay{fakeRelay: &fakeRelay{}}
	p.SetRelay(routing)
	require.Nil(t, p.recovery.recover(ctx, source, lease, time.Now()))
	require.Len(t, worker.requests, 1)
	require.True(t, worker.requests[0].RelayReplay)
	current, err := p.store.Get(ctx, lease.SessionID)
	require.NoError(t, err)
	replacement := takeoverSnapshot(t, lease.SessionID, 100)
	require.NoError(t, p.store.PutState(ctx, current, replacement))
	routing.emptyPlan = true
	outcome := p.recovery.recover(ctx, source, current, time.Now())
	require.NotNil(t, outcome)
	require.False(t, outcome.Lost)
	require.Len(t, worker.requests, 2)
	require.Equal(t, worker.states[0], worker.states[1], "confirmation must resend first bytes despite a newer checkpoint")
	require.Equal(t, worker.requests[0].RelayReplay, worker.requests[1].RelayReplay, "confirmation must resend first recovery flags")
	require.Equal(t, worker.requests[0].RelayInputReplay, worker.requests[1].RelayInputReplay)
	require.Equal(t, worker.requests[0].SequenceMargin, worker.requests[1].SequenceMargin)
	require.Equal(t, worker.requests[0].SRTCPIndexMargin, worker.requests[1].SRTCPIndexMargin)
}

func TestMoveRecoveryRebuildInterruptedMoveFencesBeforeResume(t *testing.T) {
	ctx := context.Background()
	p, a, b, r := setup(t)
	base := p.store
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	lease, err := base.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, base.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
	authorizeTestControl(t, p)
	p.store = &retainedMoveStore{Store: base}
	_, err = p.Move(ctx, id, "b")
	require.Error(t, err)
	intents, err := p.restart.store.ControlIntents(ctx)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	before, err := base.Get(ctx, id)
	require.NoError(t, err)
	require.NoError(t, p.ReleaseControl(ctx))
	// A new controller has no in-memory transfer facts or confirmed installation.
	restarted := New(r, base)
	sequence := &moveSequence{}
	recipient := &observedResumeWorker{fakeWorker: a}
	require.NoError(t, restarted.Register("a", a.addr, &sequenceWorker{Worker: recipient, sequence: sequence, role: "recipient"}))
	require.NoError(t, restarted.Register("b", b.addr, &sequenceWorker{Worker: b, sequence: sequence, role: "source"}))
	authorizeTestControl(t, restarted)
	require.NoError(t, restarted.recovery.rebuild(ctx, intents[0], nil))
	require.True(t, a.runs(id))
	require.Greater(t, recipient.opts.Lease.Epoch, before.Epoch, "restart must fence the uncertain tenure before resuming")
	require.Positive(t, recipient.opts.SequenceMargin, "lost export evidence requires checkpoint margins")
	require.Equal(t, []string{"recipient-resume"}, sequence.events, "restart must not export an unconfirmed installation")
	current, err := base.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, a.addr, current.Worker)
}
