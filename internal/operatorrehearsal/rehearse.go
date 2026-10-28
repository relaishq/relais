package operatorrehearsal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
)

type RehearseConfig struct {
	Checkout, Assets, Output, GoExecutable, Commit, HeadCommit, Event string
}

type rehearsal struct {
	c                       RehearseConfig
	report                  *Report
	topology                *Topology
	deployment              *Deployment
	runner                  Runner
	diagnostics             io.Writer
	clock                   *operatorgates.RealClock
	session                 *operatorgates.Session
	peer                    *LocalPeer
	pins                    []inventory.Pin
	process                 *operatorgates.ProcessConfig
	runtime                 *operatorgates.ProcessRuntime
	pool                    *operatorgates.EvidenceRuntime
	judge                   *recordingJudge
	box                     *operatorcaller.Mailbox
	store                   *operatorcaller.Directory
	load, bridge, work, run string
	callerBind              string
	active                  *operatorcaller.Broker
	caller                  *operatorgates.CallerRuntime
	launcher                *operatorcaller.FixedLauncher
	units                   []operatorcaller.Identity
}
type recordingJudge struct {
	pinned operatorevidence.PinnedJudge
	last   *operatorevidence.Summary
	cpu    []Calibration
}

func (j *recordingJudge) Judge(ctx context.Context, p operatorevidence.Pool) (operatorevidence.Summary, error) {
	s, e := j.pinned.Judge(ctx, p)
	if e == nil {
		j.last = &s
		j.cpu = nil
		for _, phase := range p.Phases {
			c, _ := Calibrate(phase.CallerJSONL)
			j.cpu = append(j.cpu, c)
		}
	}
	return s, e
}

type redactingWriter struct{ r *rehearsal }

func (w redactingWriter) Write(p []byte) (int, error) {
	_, e := io.WriteString(w.r.diagnostics, w.r.topology.Redact(string(p)))
	return len(p), e
}

type callerExecutor struct{ r *rehearsal }

func (e callerExecutor) Execute(ctx context.Context, script []byte, end time.Time) operatorcaller.Execution {
	bounded, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	result := e.r.runner.Run(bounded, Command{Label: "caller-service", Args: []string{"sh", "-s"}, Input: script})
	if result.Err != nil || result.Code == nil || result.Truncated || *result.Code < 0 {
		e.r.failure("caller-service", result)
		return operatorcaller.Execution{Certainty: operator.Unknown}
	}
	certainty := operator.Succeeded
	if *result.Code != 0 {
		certainty = operator.Failed
		e.r.failure("caller-service", result)
	}
	return operatorcaller.Execution{Certainty: certainty, ExitCode: *result.Code, Output: string(result.Stdout)}
}
func (r *rehearsal) failure(label string, result CommandResult) {
	topology := r.topology
	if topology == nil {
		topology = &Topology{}
	}
	_, _ = fmt.Fprintf(r.diagnostics, "step %s failed\n%s", label, topology.diagnosticStreams(result))
}
func (r *rehearsal) command(ctx context.Context, label string, args ...string) (string, error) {
	out := r.runner.Run(ctx, Command{Label: label, Args: args})
	if out.Err != nil || out.Code == nil || *out.Code != 0 || out.Truncated {
		r.failure(label, out)
		return "", errors.New("step-failed")
	}
	return string(out.Stdout), nil
}

// Rehearse runs resource ownership outside one shared gate session. After scoped
// cleanup, exits write a report if setup established the scenario identity.
func Rehearse(ctx context.Context, c RehearseConfig, diagnostics io.Writer) (Report, error) {
	return rehearse(ctx, c, diagnostics, (*rehearsal).setup)
}

func rehearse(ctx context.Context, c RehearseConfig, diagnostics io.Writer, setup func(*rehearsal, context.Context) error) (report Report, err error) {
	report = Report{SchemaVersion: 1, Commit: c.Commit, HeadCommit: c.HeadCommit, Event: c.Event, Errors: []string{}, Gates: []GateReport{}, Bounds: clocks.Bounds{FrontendControlBoundNS: 2_000_000, FrontendWorkerBoundNS: 3_000_000, DriftPPM: 100}}
	if e := os.MkdirAll(c.Output, 0700); e != nil {
		return report, e
	}
	log, e := os.OpenFile(filepath.Join(c.Output, "host-diagnostics.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return report, e
	}
	defer log.Close()
	work, e := os.MkdirTemp("", "relais-rehearsal-")
	if e != nil {
		return report, e
	}
	defer os.RemoveAll(work)
	r := &rehearsal{c: c, report: &report, runner: LocalRunner{}, diagnostics: io.MultiWriter(diagnostics, log), work: work}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		report.Cleanup = r.close(cleanup)
		if !report.Cleanup {
			report.Errors = append(report.Errors, "cleanup-failed")
		}
		if ctx.Err() != nil {
			report.Errors = append(report.Errors, "interrupted")
		}
		if r.session != nil {
			report.Latched = r.session.Latched()
		}
		report.Evaluate()
		if e := report.Save(filepath.Join(c.Output, "report.json")); e != nil {
			err = errors.Join(err, e)
		}
	}()
	if e = setup(r, ctx); e != nil {
		r.recordError("setup-failed", e)
		return report, errors.New("setup-failed")
	}
	scenario := operator.Scenario{SchemaVersion: 1, Gates: CoveredGates, Template: operator.Identity{SourceCommit: r.c.Commit, SHA256: report.TemplateSHA256}, ReleaseA: operator.Identity{SourceCommit: ReleaseACommit, SHA256: ReleaseASHA256}, ReleaseB: operator.Identity{SourceCommit: ReleaseBCommit, SHA256: ReleaseBSHA256}, Judge: operator.Identity{SourceCommit: ReleaseACommit, SHA256: r.judge.pinned.SHA256}, Caller: operator.Identity{SourceCommit: ReleaseACommit, SHA256: report.CallerSHA256}}
	raw, _ := json.Marshal(scenario)
	scenario.Digest = hash(raw)
	report.ScenarioSHA256 = scenario.Digest
	if e = r.deployment.Boot(ctx); e != nil {
		r.recordError("boot-failed", e)
		return report, errors.New("boot-failed")
	}
	adapter := &Adapter{Topology: r.topology, Containers: r.deployment.ContainerIDs(), Runner: r.runner, Diagnostics: r.diagnostics}
	if e = r.deployment.NegativeControls(ctx); e != nil {
		r.recordError("negative-controls-failed", e)
		return report, errors.New("negative-controls-failed")
	}
	if e = r.stage(ctx); e != nil {
		r.recordError("setup-failed", e)
		return report, errors.New("setup-failed")
	}
	r.clock = operatorgates.NewRealClock()
	armed := r.clock.Wall()
	init := operator.Init{Scenario: scenario, ArmedAtUnixMS: strconv.FormatInt(armed.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(armed.Add(operatorgates.SessionMaximum).UnixMilli(), 10), Capabilities: adapter.Capabilities()}
	r.session, e = operatorgates.NewSession(r.run, init, r.clock)
	if e != nil {
		report.Errors = append(report.Errors, "setup-failed")
		return report, e
	}
	r.peer = NewLocalPeer(adapter, r.run, init)
	defer r.peer.Close()
	r.process = &operatorgates.ProcessConfig{Parameters: r.parameters(), ArmedAt: armed, Trials: true, Pool: true}
	r.runtime = &operatorgates.ProcessRuntime{Session: r.session, Peer: r.peer, Config: r.process, Architecture: "x86_64", Pins: r.pins, Bounds: &report.Bounds}
	r.pool = &operatorgates.EvidenceRuntime{RunID: r.run, Judge: r.judge}
	if e = r.calibrate(ctx); e != nil || !report.CPU.Calibration.Passed {
		r.recordError("calibration-failed", e)
		return report, errors.New("calibration-failed")
	}
	sequence := r.session.RunSequence(ctx, operatorgates.SequencePorts{
		Minimum:        func(g operator.Gate) time.Duration { return time.Duration(duration(g))*time.Second + 180*time.Second },
		Prepare:        r.prepare,
		ReleaseCallers: func(ctx context.Context, _ string, end time.Duration) bool { return r.stop(ctx, end) },
		Command:        r.gate,
		Finalize: func(ctx context.Context, _ time.Duration) (*operatorgates.MediaSummary, error) {
			summary, err := r.pool.Media(ctx)
			if err != nil {
				r.report.Errors = append(r.report.Errors, "media-judge-failed")
			}
			return summary, err
		},
	})
	completed := map[operator.Gate]GateReport{}
	for _, g := range report.Gates {
		completed[g.Gate] = g
	}
	report.Gates = nil
	for i, g := range sequence.Gates {
		row := completed[g.Gate]
		row.Gate = g.Gate
		row.ProcedureResult = sequence.Procedures[i]
		row.Status = g.Status
		row.Reason = g.Reason
		report.Gates = append(report.Gates, row)
	}
	report.Pool = r.judge.last
	report.CPU.Phases = r.judge.cpu
	report.Clocks = r.runtime.ClockSamples
	return report, nil
}
func (r *rehearsal) recordError(stage string, err error) {
	r.report.Errors = append(r.report.Errors, stage)
	var cause string
	var path *os.PathError
	switch {
	case err == nil:
		cause = stage
	case errors.As(err, &path):
		cause = "local-file-unavailable"
	case errors.Is(err, context.Canceled):
		cause = "interrupted"
	case errors.Is(err, context.DeadlineExceeded):
		cause = "gate-deadline"
	default:
		cause = publicCode(err.Error())
	}
	if cause != stage {
		r.report.Errors = append(r.report.Errors, cause)
	}
	_, _ = fmt.Fprintf(r.diagnostics, "%s: %s\n", stage, cause)
}

func (r *rehearsal) setup(ctx context.Context) error {
	plan, e := readReleases(r.c.Assets)
	if e != nil {
		return e
	}
	commit, e := r.command(ctx, "tested-commit", "git", "--no-optional-locks", "-C", r.c.Checkout, "rev-parse", "HEAD")
	if r.c.Commit == "" {
		r.c.Commit = strings.TrimSpace(commit)
		r.report.Commit = r.c.Commit
	}
	if e != nil || strings.TrimSpace(commit) != r.c.Commit || !fullCommit.MatchString(r.c.Commit) {
		return errors.New("tested-commit-differs")
	}
	dirty, e := r.command(ctx, "source-state", "git", "--no-optional-locks", "-C", r.c.Checkout, "status", "--porcelain", "--untracked-files=normal")
	if e != nil || strings.TrimSpace(dirty) != "" {
		return errors.New("source-not-clean")
	}
	layout, e := DetectLayout(nil)
	if e != nil {
		return e
	}
	r.report.CPU.Layout = layout
	// Pin all current Go threads; future threads inherit a services mask. Caller
	// systemd units get their own explicit two-CPU mask and GOMAXPROCS=2.
	r.topology, e = Load(os.DirFS(r.c.Checkout), Config{Name: "relais-gates-" + strconv.Itoa(os.Getpid()), Version: plan.From.Version, Architecture: "x86_64", ArchiveSHA256: plan.From.ArchiveSHA256})
	if e != nil {
		return e
	}
	r.report.TemplateSHA256 = r.topology.TemplateSHA256
	if _, e = r.command(ctx, "driver-affinity", "taskset", "-apc", CPUList(layout.Services), strconv.Itoa(os.Getpid())); e != nil {
		return e
	}
	r.report.CPU.DriverAffinity = CPUList(layout.Services)
	r.report.CPU.Topology, e = r.command(ctx, "cpu-topology", "lscpu", "--parse=CPU,CORE,SOCKET,ONLINE")
	if e != nil {
		return e
	}
	r.bridge, e = os.Executable()
	if e != nil {
		return e
	}
	r.report.BinarySHA256, e = fileHash(r.bridge)
	if e != nil {
		return e
	}
	r.load = filepath.Join(r.work, "relais-load")
	if e = extractLoad(filepath.Join(r.c.Assets, plan.From.Archive), r.load); e != nil {
		return e
	}
	r.report.CallerSHA256, e = fileHash(r.load)
	if e != nil {
		return e
	}
	for i, pin := range []struct{ commit string }{{ReleaseACommit}, {ReleaseBCommit}} {
		pinned, x := operatorevidence.BuildPinned(ctx, r.c.Checkout, pin.commit, r.c.GoExecutable, filepath.Join(r.work, "judge-"+strconv.Itoa(i)), redactingWriter{r})
		if x != nil {
			_, _ = io.WriteString(r.diagnostics, "pinned judge preparation failed\nstdout/stderr:\n"+r.topology.Redact(x.Error())+"\n")
			return x
		}
		r.report.JudgeSHA256 = append(r.report.JudgeSHA256, pinned.SHA256)
		if i == 0 {
			r.judge = &recordingJudge{pinned: pinned}
		}
	}
	r.report.ReleaseCommits = []string{ReleaseACommit, ReleaseBCommit}
	r.report.ArchiveSHA256 = []string{ReleaseASHA256, ReleaseBSHA256}
	boot, e := os.ReadFile(filepath.Join(r.c.Checkout, "deploy/aws/boot/run.sh"))
	if e != nil {
		return e
	}
	for _, p := range []struct{ version, commit, archive, manifest string }{{plan.From.Version, plan.From.Commit, plan.From.ArchiveSHA256, plan.From.ManifestSHA256}, {plan.To.Version, plan.To.Commit, plan.To.ArchiveSHA256, plan.To.ManifestSHA256}} {
		r.pins = append(r.pins, inventory.Pin{Architecture: "x86_64", Version: p.version, Commit: p.commit, ArchiveSHA256: p.archive, ManifestSHA256: p.manifest, BootSHA256: hash(boot)})
	}
	var id [10]byte
	if _, e = rand.Read(id[:]); e != nil {
		return e
	}
	r.run = "run-" + hex.EncodeToString(id[:])
	mailbox := filepath.Join(r.work, "mailbox")
	if e = os.Mkdir(mailbox, 0700); e != nil {
		return e
	}
	r.store, e = operatorcaller.OpenDirectory(mailbox)
	if e != nil {
		return e
	}
	r.box = operatorcaller.NewMailbox(r.store)
	r.deployment, e = NewDeployment(r.topology, filepath.Join(r.work, "topology"), filepath.Join(r.c.Assets, plan.From.Archive), layout, r.runner, r.diagnostics)
	return e
}
func duration(g operator.Gate) int {
	if g == operator.RelayDeploy {
		return 600
	}
	return 1020
}
func (r *rehearsal) parameters() operatorgates.ProcessParameters {
	p := operatorgates.ProcessParameters{Control: "http://" + r.topology.Hosts["shared"].IP + ":24710", Relay: "http://" + r.topology.Hosts["relay"].IP + ":24700", Store: StoreEndpoint, StorePort: 16379, Workers: map[string]operatorgates.WorkerEndpoint{}}
	for _, w := range []string{"w1", "w2"} {
		ip := r.topology.Hosts[w].IP
		p.Workers[w] = operatorgates.WorkerEndpoint{HTTP: "http://" + ip + ":24701", Media: netip.MustParseAddrPort(ip + ":24711")}
	}
	return p
}
func (r *rehearsal) wall(end time.Duration) time.Time {
	return r.clock.Wall().Add(max(0, end-r.clock.Elapsed()))
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(max(0, d))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (r *rehearsal) ports() operatorgates.ProcedurePorts {
	return operatorgates.ProcedurePorts{Clock: r.clock, Sleep: pause, Step: r.runtime.Step, Inventory: r.runtime.Inventory, Broker: r.caller}
}

func (r *rehearsal) newCaller(ctx context.Context, g operator.Gate, seconds int, id string, end time.Duration) error {
	if r.callerBind == "" {
		route, err := r.command(ctx, "caller-route", "ip", "-j", "route", "get", r.topology.Hosts["relay"].IP)
		if err != nil {
			return err
		}
		var routes []struct {
			Source string `json:"prefsrc"`
		}
		if json.Unmarshal([]byte(route), &routes) != nil || len(routes) != 1 {
			return errors.New("caller-route-invalid")
		}
		ip, err := netip.ParseAddr(routes[0].Source)
		if err != nil || !ip.Is4() || ip.IsUnspecified() {
			return errors.New("caller-route-invalid")
		}
		r.callerBind = netip.AddrPortFrom(ip, 0).String()
		_, _ = fmt.Fprintln(r.diagnostics, "caller-bind="+r.callerBind)
	}
	now := r.clock.Wall()
	deadline := r.wall(end)
	if maximum := now.Add(20 * time.Minute); maximum.Before(deadline) {
		deadline = maximum
	}
	identity := operatorcaller.Identity{RunID: r.run, Gate: g, Phase: operatorcaller.Survivor, CallerID: id, Attempt: "1", PublicSHA: ReleaseACommit}
	m := operatorcaller.Manifest{Version: 1, Identity: identity, ArmedAt: r.process.ArmedAt.UnixMilli(), CreatedAt: now.UnixMilli(), Deadline: deadline.UnixMilli(), Plan: operatorcaller.NewPlan(operatorcaller.Survivor, seconds, ReleaseACommit), Endpoints: operatorcaller.Endpoints{RunID: r.run, Signaling: SignalingURL + "/calls", Relay: r.topology.Hosts["relay"].IP + ":24780", FrontendControlBoundNS: r.report.Bounds.FrontendControlBoundNS, FrontendWorkerBoundNS: r.report.Bounds.FrontendWorkerBoundNS, DriftPPM: int(r.report.Bounds.DriftPPM)}}
	b, e := operatorcaller.NewBroker(m, r.box, operatorcaller.SystemClock{}, r.session)
	if e != nil {
		return e
	}
	config := operatorcaller.BridgeConfig{Identity: identity, Bind: r.callerBind, CertificateFile: filepath.Join(r.deployment.Work, "tls/signal/server.crt"), LoadSHA256: r.report.CallerSHA256}
	raw, e := json.Marshal(config)
	if e != nil {
		return e
	}
	path := filepath.Join(r.work, "config-"+id+".json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		return e
	}
	launch := operatorcaller.LaunchConfig{BridgePath: r.bridge, BridgeSHA256: r.report.BinarySHA256, LoadPath: r.load, LoadSHA256: r.report.CallerSHA256, ConfigPath: path, ConfigSHA256: hash(raw), MailboxPath: filepath.Join(r.work, "mailbox"), WorkRoot: r.work, Architecture: "x86_64", RuntimeDeadline: deadline, CPUAffinity: strings.ReplaceAll(CPUList(r.report.CPU.Layout.Caller), ",", " ")}
	r.launcher, e = operatorcaller.NewFixedLauncher(map[operatorcaller.Identity]operatorcaller.LaunchConfig{identity: launch}, callerExecutor{r}, r.box, r.session)
	if e != nil {
		return e
	}
	r.active = b
	r.units = append(r.units, identity)
	return nil
}
func (r *rehearsal) startCaller(ctx context.Context, g operator.Gate, seconds int, id string, end, gateEnd time.Duration) error {
	if err := r.newCaller(ctx, g, seconds, id, gateEnd); err != nil {
		return err
	}
	err := operatorcaller.PrepareCallers(ctx, r.launcher, r.active, nil, r.topology.Secrets["SignalToken"], r.wall(end), func(context.Context) error { return nil })
	if err == nil {
		err = r.affinity(ctx)
	}
	if err != nil {
		r.callerDiagnostics(ctx, r.active.Manifest.Identity)
	}
	return err
}
func (r *rehearsal) prepare(ctx context.Context, g operator.Gate, end, gateEnd time.Duration) error {
	// Previous gate must have confirmed stop before a successor can dial.
	if r.active != nil && !r.stop(ctx, end) {
		return &operatorgates.ProcedureError{Code: "command-result-unknown"}
	}
	r.process.Target = ""
	r.process.Operation = ""
	r.process.Mixed = false
	r.runtime.BeforeMutation = nil
	if e := operatorgates.GatePrecondition(ctx, r.process, r.ports(), end); e != nil {
		return e
	}
	if e := r.startCaller(ctx, g, duration(g), strconv.Itoa(len(r.units)+1), end, gateEnd); e != nil {
		_ = r.stop(ctx, end)
		return e
	}
	inventory, e := r.session.FreshInventory(ctx, r.peer, r.pins, end, ReleaseACommit, true)
	if e != nil {
		return e
	}
	if e = r.runtime.VerifyClocks(ctx, end); e != nil {
		return e
	}
	phase := operatorevidence.Phase{DeployedSHA: ReleaseACommit, Clocks: r.runtime.ClockSamples[len(r.runtime.ClockSamples)-1], Inventory: inventory}
	r.caller = operatorgates.NewCallerRuntime(r.active, r.clock, gateEnd, r.pool, phase)
	r.runtime.BeforeMutation = r.caller.MarkFault
	return nil
}
func (r *rehearsal) affinity(ctx context.Context) error {
	mask, e := r.command(ctx, "caller-affinity", "taskset", "-pc", strconv.Itoa(r.active.CallerPID))
	if e != nil {
		return e
	}
	_, value, ok := strings.Cut(strings.TrimSpace(mask), ": ")
	if !ok {
		return errors.New("caller-affinity-unavailable")
	}
	cpus, e := ParseCPUList(value)
	if e != nil || CPUList(cpus) != CPUList(r.report.CPU.Layout.Caller) {
		return errors.New("caller-affinity-differs")
	}
	r.report.CPU.CallerAffinities = append(r.report.CPU.CallerAffinities, CPUList(cpus))
	return nil
}
func (r *rehearsal) calibrate(ctx context.Context) error {
	if e := r.startCaller(ctx, operator.Rollout, 60, "1", r.clock.Elapsed()+time.Minute, r.clock.Elapsed()+4*time.Minute); e != nil {
		return e
	}
	raw, e := r.active.Collect(ctx, r.wall(r.clock.Elapsed()+3*time.Minute))
	if e != nil {
		r.callerDiagnostics(ctx, r.active.Manifest.Identity)
		return e
	}
	r.report.CPU.Calibration, e = Calibrate(raw)
	if e != nil || !r.report.CPU.Calibration.Passed {
		r.callerDiagnostics(ctx, r.active.Manifest.Identity)
	}
	if !r.stop(ctx, r.clock.Elapsed()+time.Minute) {
		return errors.New("caller-stop-unconfirmed")
	}
	return e
}
func (r *rehearsal) stop(ctx context.Context, end time.Duration) bool {
	if r.active == nil {
		return true
	}
	stop := r.launcher.Stop(ctx, r.active.Manifest.Identity, r.wall(end))
	if stop.Inactive && stop.Certainty == operator.Succeeded {
		r.active = nil
		return true
	}
	r.callerDiagnostics(ctx, r.active.Manifest.Identity)
	return false
}
func (r *rehearsal) gate(ctx context.Context, g operator.Gate, end time.Duration) operatorgates.ProcedureResult {
	r.caller.End = end
	p := r.ports()
	p.Media = r.caller.Media
	p.Duplicates = r.caller.Duplicates
	var result operatorgates.ProcedureResult
	if g == operator.UpgradeRollback {
		result = operatorgates.RunReleaseGate(ctx, r.process, p, end)
	} else {
		result = operatorgates.RunProcessGate(ctx, g, r.process, p, end)
	}
	if result.Status == operator.Failure {
		r.callerDiagnostics(ctx, r.active.Manifest.Identity)
	}
	stopped := r.stop(ctx, min(r.clock.Elapsed()+time.Minute, end))
	result = operatorgates.Stopped(stopped, result)
	r.report.Gates = append(r.report.Gates, GateReport{Gate: g, ProcedureResult: result, CallerStopped: stopped})
	return result
}
func (r *rehearsal) callerDiagnostics(ctx context.Context, id operatorcaller.Identity) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	script := "systemctl show " + quote(id.Unit()) + " -p Id -p ActiveState -p SubState -p MainPID -p Result -p ExecMainCode -p ExecMainStatus -p User -p Group -p ProtectHome -p PrivateTmp -p RootDirectory -p WorkingDirectory -p CPUAffinity -p RuntimeMaxUSec\njournalctl -u " + quote(id.Unit()) + " --no-pager -o cat -n 60\n"
	out := r.runner.Run(bounded, Command{Label: "caller-diagnostics", Args: []string{"sh", "-s"}, Input: []byte(script)})
	_, _ = fmt.Fprintf(r.diagnostics, "caller %s\n%s", id.Unit(), r.topology.diagnosticStreamsLimit(out, commandOutputLimit("caller-diagnostics")))
}
func (r *rehearsal) close(ctx context.Context) bool {
	ok := true
	if r.topology != nil {
		for _, i := range r.units {
			r.callerDiagnostics(ctx, i)
			_, _ = r.command(ctx, "cleanup-caller", "systemctl", "stop", i.Unit())
			// systemd can collect a completed transient unit before this stop.
			// Cleanup is a separate resource owner: confirm inactivity without
			// changing the gate session's earlier stop certainty or fatal latch.
			read, _ := operatorcaller.InactiveScript(i)
			end, _ := ctx.Deadline()
			if end.IsZero() {
				end = time.Now().Add(time.Minute)
			}
			if !operatorcaller.ObservedInactive((callerExecutor{r}).Execute(ctx, read, end)) {
				ok = false
			}
		}
	}
	if r.deployment != nil {
		// Only fixed unit state, PIDs and chrony diagnostics are uploaded. No unit
		// environments, credential files, command lines or raw mailbox are collected.
		for slot, id := range r.deployment.ContainerIDs() {
			if slot != "relay" && slot != "w1" && slot != "w2" && slot != "shared" {
				continue
			}
			out := r.runner.Run(ctx, Command{Label: "host-diagnostics", Args: []string{"docker", "exec", id, "sh", "-c", "systemctl show 'relais-*' -p Id -p ActiveState -p SubState -p MainPID; chronyc -n tracking; chronyc -n sources"}})
			_, _ = fmt.Fprintf(r.diagnostics, "host %s\n%s", slot, r.topology.diagnosticStreams(out))
		}
		if r.deployment.Close(ctx) != nil {
			ok = false
		}
	}
	if r.store != nil {
		_ = r.store.Close()
	}
	return ok
}
