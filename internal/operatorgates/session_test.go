package operatorgates

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func fixtureSession(t *testing.T) (*Session, *FakeClock) {
	t.Helper()
	c := &FakeClock{Now: time.UnixMilli(1000000)}
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: "1000000", EndUnixMS: "7300000", Capabilities: []operator.Operation{operator.HostExecute, operator.MailboxCreate, operator.MailboxRead, operator.CallerStart, operator.CallerStop}}
	s, err := NewSession("session", init, c)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}
func issue(t *testing.T, s *Session, op operator.Operation, action string) operator.Request {
	t.Helper()
	r, err := s.Issue(op, "shared", action, json.RawMessage(`{}`), nil, s.clock.Elapsed()+time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func reply(r operator.Request, c operator.Certainty) operator.Reply {
	zero := 0
	return operator.Reply{ID: r.ID, Transport: c, ExitCode: &zero}
}
func TestUnknownLatches(t *testing.T) {
	s, _ := fixtureSession(t)
	r := issue(t, s, operator.HostExecute, "move")
	_, err := s.Accept(s.ID, reply(r, operator.Unknown), operator.Succeeded)
	if err != nil || !s.Latched() {
		t.Fatal("MUTATION: unknown-latch")
	}
}
func TestLatchFencesEffects(t *testing.T) {
	for _, op := range []operator.Operation{operator.HostExecute, operator.MailboxCreate, operator.CallerStart, operator.CallerStop} {
		s, _ := fixtureSession(t)
		s.Halt("command-result-unknown")
		if _, err := s.Issue(op, "shared", "restore", nil, nil, time.Minute); err == nil {
			t.Fatal("MUTATION: latch-fence")
		}
	}
}
func TestPendingIdentity(t *testing.T) {
	s, c := fixtureSession(t)
	r := issue(t, s, operator.CallerStart, "")
	c.Advance(time.Minute)
	s.Expire()
	final := s.Final(nil)
	if len(final.Pending) != 1 || final.Pending[0] != r.ID {
		t.Fatal("MUTATION: pending-identity")
	}
	if final.Complete || final.Status == operator.Passed || !s.Latched() {
		t.Fatal("MUTATION: pending-final")
	}
}
func TestLateUnknown(t *testing.T) {
	s, c := fixtureSession(t)
	r := issue(t, s, operator.MailboxCreate, "")
	c.Advance(time.Minute)
	s.Expire()
	got, err := s.Accept(s.ID, reply(r, operator.Unknown), operator.Unknown)
	if err != nil || got != operator.Unknown || !s.Latched() {
		t.Fatal("MUTATION: late-unknown")
	}
	if _, err = s.Issue(operator.HostExecute, "shared", "restore", nil, nil, 2*time.Minute); err == nil {
		t.Fatal("MUTATION: late-unknown")
	}
}
func TestLateSuccessCannotPass(t *testing.T) {
	s, c := fixtureSession(t)
	r := issue(t, s, operator.HostExecute, "move")
	c.Advance(time.Minute)
	got, _ := s.Accept(s.ID, reply(r, operator.Succeeded), operator.Succeeded)
	if got != operator.Unknown || !s.Latched() {
		t.Fatal("MUTATION: late-success")
	}
}
func TestDeadlineBeforeSubmission(t *testing.T) {
	s, c := fixtureSession(t)
	end := c.Elapsed() + time.Minute
	c.Advance(time.Minute)
	if _, err := s.Issue(operator.HostExecute, "shared", "move", nil, nil, end); err == nil {
		t.Fatal("MUTATION: submission-deadline")
	}
	if len(s.requests) != 0 {
		t.Fatal("expired request allocated")
	}
}
func TestDeadlineBudgets(t *testing.T) {
	s, c := fixtureSession(t)
	c.Advance(100 * time.Minute)
	end, ok := s.Budget.GateEnd()
	if !ok || end != SessionMaximum {
		t.Fatal("MUTATION: lifetime-clamp")
	}
	c.Advance(5 * time.Minute)
	if _, ok = s.Budget.GateEnd(); ok {
		t.Fatal("MUTATION: gate-cutoff")
	}
}
func TestPoolBudget(t *testing.T) {
	s, c := fixtureSession(t)
	c.Advance(104 * time.Minute)
	if s.Budget.PoolEnd() != SessionMaximum {
		t.Fatal("MUTATION: pool-clamp")
	}
	if s.Budget.MutationEnd() != MutationWindow {
		t.Fatal("mutation cutoff changed")
	}
}
func TestWallClockCannotExtend(t *testing.T) {
	s, c := fixtureSession(t)
	c.Advance(100 * time.Minute)
	c.Now = c.Now.Add(-time.Hour)
	if s.Budget.Remaining() != 5*time.Minute {
		t.Fatal("wall step extended budget")
	}
	end, _ := s.Budget.GateEnd()
	absolute, remaining := s.Budget.Wire(end)
	if absolute != "7300000" || remaining != 300000 {
		t.Fatal("wire budget diverged")
	}
}
func TestCertaintyLevels(t *testing.T) {
	for _, tt := range []struct {
		name, action              string
		transport, evidence, want operator.Certainty
		exit                      *int
		refused, latch            bool
	}{
		{"success", "move", operator.Succeeded, operator.Succeeded, operator.Succeeded, new(int), false, false},
		{"exit-zero-unknown", "move", operator.Succeeded, operator.Unknown, operator.Unknown, new(int), false, true},
		{"mutation-exit-failed", "move", operator.Failed, operator.Failed, operator.Unknown, intPointer(3), false, true},
		{"pure-unknown", "worker-empty", operator.Unknown, operator.Succeeded, operator.Unknown, nil, false, true},
		{"pure-failed", "worker-empty", operator.Failed, operator.Failed, operator.Failed, intPointer(3), false, false},
		{"impure-status", "status", operator.Failed, operator.Failed, operator.Unknown, intPointer(3), false, true},
		{"proved-refusal", "move", operator.Failed, operator.Unknown, operator.Failed, nil, true, false},
		{"missing-exit", "move", operator.Succeeded, operator.Succeeded, operator.Unknown, nil, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := fixtureSession(t)
			r := issue(t, s, operator.HostExecute, tt.action)
			got, err := s.Accept(s.ID, operator.Reply{ID: r.ID, Transport: tt.transport, ExitCode: tt.exit, NotSubmitted: tt.refused, RefusalCode: "invalid-request"}, tt.evidence)
			if err != nil || got != tt.want || s.Latched() != tt.latch {
				t.Fatalf("certainty: %s latch=%v err=%v", got, s.Latched(), err)
			}
		})
	}
}
func intPointer(v int) *int { return &v }
func TestDuplicateMismatchAndEOF(t *testing.T) {
	for _, mode := range []string{"duplicate", "mismatch", "eof"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixtureSession(t)
			r := issue(t, s, operator.HostExecute, "move")
			switch mode {
			case "duplicate":
				_, _ = s.Accept(s.ID, reply(r, operator.Succeeded), operator.Succeeded)
				_, _ = s.Accept(s.ID, reply(r, operator.Succeeded), operator.Succeeded)
			case "mismatch":
				_, _ = s.Accept("other", reply(r, operator.Succeeded), operator.Succeeded)
			case "eof":
				s.TransportLost()
			}
			if !s.Latched() {
				t.Fatal("protocol uncertainty did not latch")
			}
		})
	}
}
func TestSerialEffectsAndBoundedPureReads(t *testing.T) {
	s, _ := fixtureSession(t)
	issue(t, s, operator.HostExecute, "move")
	issue(t, s, operator.HostExecute, "worker-status")
	if _, err := s.Issue(operator.HostExecute, "shared", "restore", nil, nil, time.Minute); err == nil {
		t.Fatal("overlapping effects")
	}
}
func TestKnownFailureDominates(t *testing.T) {
	s, _ := fixtureSession(t)
	s.Halt("command-result-unknown")
	final := s.Final([]operator.GateResult{{Gate: operator.Rollout, Status: operator.Failure}})
	if final.Status != operator.Failure || final.Complete {
		t.Fatal("known failure hidden by unknown")
	}
}
func TestTraceSanitization(t *testing.T) {
	s, _ := fixtureSession(t)
	s.Halt("raw error containing confidential data")
	if s.trace[0].Code != "unclassified" {
		t.Fatal("MUTATION: trace-sanitization")
	}
}

func TestMailboxUnknownImmediately(t *testing.T) {
	s, _ := fixtureSession(t)
	r := issue(t, s, operator.MailboxCreate, "")
	_, _ = s.Accept(s.ID, reply(r, operator.Unknown), operator.Unknown)
	if !s.Latched() {
		t.Fatal("MUTATION: mailbox-latch")
	}
}
func TestUnresolvedCannotPass(t *testing.T) {
	s, _ := fixtureSession(t)
	issue(t, s, operator.HostExecute, "move")
	final := s.Final([]operator.GateResult{{Gate: operator.Rollout, Status: operator.Passed}})
	if final.Complete || final.Status == operator.Passed {
		t.Fatal("MUTATION: unresolved-final")
	}
}

func TestGateLateResult(t *testing.T) {
	s, c := fixtureSession(t)
	end := time.Minute
	c.Advance(end)
	r := s.CompleteGate(operator.GateResult{Gate: operator.Rollout, Status: operator.Passed}, end)
	if r.Status != operator.Inconclusive || r.Reason != "gate-deadline" {
		t.Fatal("MUTATION: gate-late-result")
	}
	r = s.CompleteGate(operator.GateResult{Gate: operator.Rollout, Status: operator.Failure}, end)
	if r.Status != operator.Failure {
		t.Fatal("known failure lost at deadline")
	}
}
func TestGateUnknownStops(t *testing.T) {
	s, _ := fixtureSession(t)
	s.CompleteGate(operator.GateResult{Gate: operator.Rollout, Status: operator.Inconclusive, Reason: "command-result-unknown"}, time.Minute)
	if !s.Latched() {
		t.Fatal("MUTATION: gate-unknown-stop")
	}
}

func TestPublicCategoryAllowlist(t *testing.T) {
	for _, input := range []struct{ name, message string }{
		{"Error", "customer-production-db"}, {"CustomerProductionDatabaseError", "error"}, {"Error", "precondition-failed:customer-production-db"}, {"Error", "Caller preparation deadline:customer-production-db"},
	} {
		if ErrorCategory(input.name, input.message) != "unclassified" {
			t.Fatal("MUTATION: public-code")
		}
		s, _ := fixtureSession(t)
		s.Halt(input.message)
		final := s.Final([]operator.GateResult{{Gate: operator.Rollout, Status: operator.Inconclusive, Reason: input.message}})
		raw, _ := json.Marshal(final)
		if strings.Contains(string(raw), "customer-production-db") || strings.Contains(string(raw), "CustomerProductionDatabaseError") {
			t.Fatal("private identifier leaked into final artifact")
		}
	}
}
func TestScenarioDigestBoundary(t *testing.T) {
	for _, digest := range []string{"", "customer-production-db", strings.Repeat("g", 64), strings.Repeat("A", 64), strings.Repeat("a", 64)} {
		init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: digest, Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: "1000000", EndUnixMS: "7300000"}
		s, err := NewSession("s", init, &FakeClock{Now: time.UnixMilli(1000000)})
		if digest != strings.Repeat("a", 64) {
			if err == nil {
				t.Fatal("MUTATION: scenario-digest")
			}
			continue
		}
		if err != nil || s.Final(nil).ScenarioDigest != digest {
			t.Fatal("validated digest not retained")
		}
	}
}
func TestSessionArtifactDeterminism(t *testing.T) {
	var reference []byte
	for n := 0; n < 32; n++ {
		s, c := fixtureSession(t)
		for i := 0; i < 13; i++ {
			issue(t, s, operator.HostExecute, "worker-status")
		}
		c.Advance(time.Minute)
		s.Expire()
		raw, err := json.Marshal(s.Final(nil))
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			reference = raw
		} else if !bytes.Equal(reference, raw) {
			t.Fatal("session replay trace changed with map iteration")
		}
	}
}
