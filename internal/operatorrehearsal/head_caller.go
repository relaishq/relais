package operatorrehearsal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

// HeadCallerEvidence is the head-caller phase's sanitized media evidence file.
const HeadCallerEvidence = "head-caller-evidence.json"

// HeadCallerReport is a diagnostic phase: rollout repeated once with the
// relais-load built from the tested checkout. It has its own session, spool,
// judge copy and evidence file. Report.Evaluate never reads it.
type HeadCallerReport struct {
	// Commit is the tested checkout that built the caller. The scenario's caller
	// source commit stays the deployed release, as caller plans require.
	Commit           string                 `json:"commit"`
	CallerSHA256     string                 `json:"callerSha256"`
	ScenarioSHA256   string                 `json:"scenarioSha256"`
	Gates            []GateReport           `json:"gates"`
	CallerAffinities []string               `json:"callerAffinities"`
	CallerTiming     []CallerTiming         `json:"callerTiming"`
	Uncertainties    []operator.Uncertainty `json:"uncertainties"`
	Latched          bool                   `json:"latched"`
	Errors           []string               `json:"errors"`
}

// runHeadCaller runs after the pinned sequence's report inputs are final. It
// shares only the deployment and teardown with the pinned comparison.
func (r *rehearsal) runHeadCaller(ctx context.Context, scenario operator.Scenario) *HeadCallerReport {
	head := &HeadCallerReport{Commit: r.c.Commit, Gates: []GateReport{}, CallerAffinities: []string{}, CallerTiming: []CallerTiming{}, Uncertainties: []operator.Uncertainty{}, Errors: []string{}}
	_, _ = fmt.Fprintln(r.diagnostics, "head-caller-phase begin")
	defer func() { _, _ = fmt.Fprintln(r.diagnostics, "head-caller-phase end") }()
	fail := func(code string) *HeadCallerReport {
		head.Errors = append(head.Errors, code)
		_, _ = fmt.Fprintln(r.diagnostics, "head-caller-phase failed: "+code)
		return head
	}
	if r.session == nil || r.session.Latched() {
		return fail("prior-gate-stopped")
	}
	// make operator-rehearsal-build places it beside this executable.
	load := callerBinary{path: filepath.Join(r.work, "relais-load-head")}
	err := copyExecutable(filepath.Join(filepath.Dir(r.bridge), "relais-load"), load.path)
	if err == nil {
		load.sha256, err = fileHash(load.path)
	}
	if err != nil {
		return fail("local-file-unavailable")
	}
	head.CallerSHA256 = load.sha256
	// As after calibration, prove the pinned gates released every call and
	// session before a new caller starts on the shared deployment.
	if err = r.verifyIdleAt(ctx, r.clock.Elapsed()+2*time.Minute, "head-caller"); err != nil {
		return fail("precondition-failed")
	}
	scenario.Gates = []operator.Gate{operator.Rollout}
	scenario.Caller.SHA256 = load.sha256
	scenario.Digest = ""
	raw, _ := json.Marshal(scenario)
	scenario.Digest = hash(raw)
	head.ScenarioSHA256 = scenario.Digest
	armed := r.clock.Wall()
	init := operator.Init{Scenario: scenario, ArmedAtUnixMS: strconv.FormatInt(armed.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(armed.Add(operatorgates.SessionMaximum).UnixMilli(), 10)}
	judge := r.headJudge(head)
	if err = r.runResourceSequence(ctx, init, headSequence(head, judge, load)); err != nil {
		fail("sequence-failed")
	}
	if judge.timing != nil {
		head.CallerTiming = judge.timing
	}
	return head
}

// headJudge is the head phase's judge copy. Its evidence file names the head
// caller build, which the pool's release source commit does not.
func (r *rehearsal) headJudge(head *HeadCallerReport) *recordingJudge {
	pinned := r.judge.pinned
	return &recordingJudge{pinned: pinned, diagnostics: r.diagnostics, retain: func(p operatorevidence.Pool) error {
		return saveMediaEvidence(r.c.Output, HeadCallerEvidence, p, pinned.SHA256, &headCallerBuild{head.Commit, head.CallerSHA256})
	}}
}

func headSequence(head *HeadCallerReport, judge *recordingJudge, load callerBinary) resourceSequence {
	return resourceSequence{spool: "head-session-spool", judge: judge, binary: load, affinities: &head.CallerAffinities,
		started: func(*operatorgates.Session) {},
		finished: func(s *operatorgates.Session, result operatorgates.ResourceResult) {
			if rows := gateRows(nil, result.SequenceResult, result.CallerStopped); rows != nil {
				head.Gates = rows
			}
			if result.PoolError {
				head.Errors = append(head.Errors, "media-judge-failed")
			}
			head.Latched = s.Latched()
			head.Uncertainties = append([]operator.Uncertainty{}, s.Uncertainties()...)
		}}
}

func copyExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// sanitized applies the pinned report's label and digest rules.
func (h HeadCallerReport) sanitized() HeadCallerReport {
	if !fullCommit.MatchString(h.Commit) {
		h.Commit = "unavailable"
	}
	for _, p := range []*string{&h.CallerSHA256, &h.ScenarioSHA256} {
		if *p != "" && !fullDigest.MatchString(*p) {
			*p = "unavailable"
		}
	}
	h.Gates = sanitizeGates(h.Gates)
	h.CallerAffinities = append([]string{}, h.CallerAffinities...)
	for i, s := range h.CallerAffinities {
		h.CallerAffinities[i] = publicCPUList(s)
	}
	h.CallerTiming = sanitizeCallerTiming(h.CallerTiming)
	h.Uncertainties = append([]operator.Uncertainty{}, h.Uncertainties...)
	for i := range h.Uncertainties {
		h.Uncertainties[i] = h.Uncertainties[i].Public()
	}
	h.Errors = append([]string{}, h.Errors...)
	for i, s := range h.Errors {
		h.Errors[i] = publicCode(s)
	}
	return h
}
