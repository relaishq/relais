package operatorrehearsal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// resourceFence belongs to the launcher's one-shot reservations. Only the
// session on the other pipe owns the gate latch and its public uncertainty.
type resourceFence struct{ latched bool }

func (f *resourceFence) Latched() bool { return f.latched }
func (f *resourceFence) Halt(string)   { f.latched = true }

type callerResources struct {
	r        *rehearsal
	identity operatorcaller.Identity
	launcher *operatorcaller.FixedLauncher
	manifest operatorcaller.Manifest
}

func resourceReply(id string, c operator.Certainty, payload any) operator.Reply {
	code := 0
	raw, _ := json.Marshal(payload)
	return operator.Reply{ID: id, Transport: c, ExitCode: &code, Payload: raw}
}
func (c *callerResources) Ready(ctx context.Context, req operator.Request) operator.Reply {
	var args operatorgates.CallerRequest
	if observe.Decode(req.Args, &args, observe.Contract{"manifest"}) != nil || args.Manifest.Validate(args.Manifest.Identity, time.Now()) != nil {
		return refusal(req.ID, "invalid-request")
	}
	if c.launcher != nil && c.identity == args.Manifest.Identity {
		return refusal(req.ID, "invalid-request")
	}
	// Fixed launch configuration remains resource-owned. It receives the public
	// immutable manifest, not a gate procedure callback.
	if err := c.r.configureCaller(ctx, args.Manifest, &resourceFence{}); err != nil {
		return refusal(req.ID, "invalid-request")
	}
	c.launcher = c.r.launcher
	c.identity = args.Manifest.Identity
	c.manifest = args.Manifest
	end, _ := ctx.Deadline()
	certainty := c.launcher.Ready(ctx, end)
	return resourceReply(req.ID, certainty, operatorgates.CallerProof{Ready: certainty == operator.Succeeded})
}
func (c *callerResources) matches(req operator.Request) bool {
	var args operatorgates.CallerRequest
	a, _ := json.Marshal(c.manifest)
	if observe.Decode(req.Args, &args, observe.Contract{"manifest"}) != nil {
		return false
	}
	b, _ := json.Marshal(args.Manifest)
	return c.launcher != nil && string(a) == string(b)
}
func (c *callerResources) Start(ctx context.Context, req operator.Request) operator.Reply {
	if !c.matches(req) {
		return refusal(req.ID, "invalid-request")
	}
	end, _ := ctx.Deadline()
	started := c.launcher.Start(ctx, c.identity, c.identity.Phase, end)
	if started.State == operatorcaller.NotStarted && started.Certainty == operator.Failed {
		return refusal(req.ID, "deadline-before-submission")
	}
	if started.Certainty == operator.Succeeded {
		pid, err := c.r.command(ctx, "caller-affinity", "systemctl", "show", "--value", "-p", "MainPID", c.identity.Unit())
		process, parseErr := strconv.Atoi(strings.TrimSpace(pid))
		if err != nil || parseErr != nil || process <= 0 {
			c.r.callerDiagnostics(ctx, c.identity)
			return resourceReply(req.ID, operator.Unknown, operatorgates.CallerProof{})
		}
		out, err := c.r.command(ctx, "caller-affinity", "taskset", "-pc", strconv.Itoa(process))
		_, mask, ok := strings.Cut(strings.TrimSpace(out), ": ")
		cpus, parseErr := ParseCPUList(mask)
		if err != nil || !ok || parseErr != nil || CPUList(cpus) != CPUList(c.r.report.CPU.Layout.Caller) {
			c.r.callerDiagnostics(ctx, c.identity)
			return resourceReply(req.ID, operator.Unknown, operatorgates.CallerProof{})
		}
		c.r.report.CPU.CallerAffinities = append(c.r.report.CPU.CallerAffinities, CPUList(cpus))
	} else {
		c.r.callerDiagnostics(ctx, c.identity)
	}
	return resourceReply(req.ID, started.Certainty, operatorgates.CallerProof{Start: started.State})
}
func (c *callerResources) Stop(ctx context.Context, req operator.Request) operator.Reply {
	if !c.matches(req) {
		return refusal(req.ID, "invalid-request")
	}
	end, _ := ctx.Deadline()
	stopped := c.launcher.Stop(ctx, c.identity, end)
	if stopped.Certainty != operator.Succeeded {
		c.r.callerDiagnostics(ctx, c.identity)
	}
	return resourceReply(req.ID, stopped.Certainty, operatorgates.CallerProof{Inactive: stopped.Inactive})
}

type mailboxResources struct {
	store operatorcaller.Store
	spool *os.Root
}

func (m mailboxResources) Read(ctx context.Context, req operator.Request) operator.Reply {
	var args operatorcaller.MailboxArgs
	if observe.Decode(req.Args, &args, observe.Contract{"key"}) != nil || len(args.Data) != 0 || args.Blob != nil {
		return refusal(req.ID, "invalid-request")
	}
	value, err := m.store.Read(ctx, args.Key)
	if err != nil {
		return operator.Reply{ID: req.ID, Transport: operator.Unknown}
	}
	reply := resourceReply(req.ID, operator.Succeeded, operatorcaller.MailboxValue{Found: &value.Found, Data: value.Data})
	if len(reply.Payload) < operator.MaxFrameBytes/2 {
		return reply
	}
	// Reuse the existing reply blob envelope. The session verifies the entire
	// MailboxValue before its operation classifier observes any bytes.
	sum := sha256.Sum256(reply.Payload)
	name := "mailbox-" + req.ID + "-" + hex.EncodeToString(sum[:8])
	f, err := m.spool.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return operator.Reply{ID: req.ID, Transport: operator.Unknown}
	}
	_, err = f.Write(reply.Payload)
	err = errors.Join(err, f.Close())
	if err != nil {
		return operator.Reply{ID: req.ID, Transport: operator.Unknown}
	}
	reply.Blob = &operator.Blob{Name: name, Size: int64(len(reply.Payload)), SHA256: hex.EncodeToString(sum[:])}
	reply.Payload = nil
	return reply
}
func (m mailboxResources) Create(ctx context.Context, req operator.Request) operator.Reply {
	var args operatorcaller.MailboxArgs
	if observe.Decode(req.Args, &args, observe.Contract{"key", "data"}) != nil || args.Blob != nil {
		return refusal(req.ID, "invalid-request")
	}
	state := m.store.Create(ctx, args.Key, args.Data)
	certainty := operator.Succeeded
	if state == operatorcaller.CreateUnknown {
		certainty = operator.Unknown
	}
	return resourceReply(req.ID, certainty, operatorcaller.MailboxValue{State: state})
}

// runResourceSequence is the rehearsal's only gate entry. Both ends exchange
// real JSON frames through pipes, using the same ServeSequence and ResourceRunner
// as the session command. Setup/calibration and owner teardown remain outside.
func (r *rehearsal) runResourceSequence(ctx context.Context, init operator.Init) error {
	spoolDir := filepath.Join(r.work, "session-spool")
	if err := os.Mkdir(spoolDir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(spoolDir)
	if err != nil {
		return err
	}
	defer root.Close()
	adapter := &Adapter{Topology: r.topology, Containers: r.deployment.ContainerIDs(), Runner: r.runner, Diagnostics: r.diagnostics, Caller: &callerResources{r: r}, Mailbox: mailboxResources{store: r.store, spool: root}}
	init.Capabilities = adapter.Capabilities()
	config := operatorgates.ResourceConfig{Parameters: r.parameters(), Pins: r.pins, Bounds: r.report.Bounds, Architecture: "x86_64", Token: r.topology.Secrets["SignalToken"], JudgeBinary: r.judge.pinned.Binary, CallerIDBase: uint64(len(r.units) + 1), Endpoints: operatorcaller.Endpoints{RunID: r.run, Signaling: SignalingURL + "/calls", Relay: r.topology.Hosts["relay"].IP + ":24780", FrontendControlBoundNS: r.report.Bounds.FrontendControlBoundNS, FrontendWorkerBoundNS: r.report.Bounds.FrontendWorkerBoundNS, DriftPPM: int(r.report.Bounds.DriftPPM)}}
	runner := operatorgates.ResourceRunner{Config: config, Judge: r.judge, Started: func(s *operatorgates.Session) {
		r.session = s
		r.judge.note = s.RecordUncertainty
		s.SetObservationFailureSink(r.preconditionDiagnostic)
		s.SetUncertaintySink(func(u operator.Uncertainty) {
			raw, _ := json.Marshal(u)
			_, _ = io.WriteString(r.diagnostics, "command-result-unknown "+string(raw)+"\n")
		})
	}, Finished: func(_ *operatorgates.Session, result operatorgates.ResourceResult) {
		r.recordSequence(result.SequenceResult, result.CallerStopped)
		if result.PoolError {
			r.report.Errors = append(r.report.Errors, "media-judge-failed")
		}
		r.report.Clocks = result.Clocks
		if r.runtime != nil {
			r.report.Clocks = append(append([]clocks.Evidence{}, r.runtime.ClockSamples...), result.Clocks...)
		}
	}}
	engineRead, serverWrite := io.Pipe()
	serverRead, engineWrite := io.Pipe()
	engine := operatortransport.NewPipe(engineRead, engineWrite)
	server := operatortransport.NewPipe(serverRead, serverWrite)
	defer engine.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- operatorgates.ServeSequence(ctx, engine, operator.Hello{Protocol: operator.ProtocolVersion}, runner.Run, root)
	}()
	final, err := adapter.Serve(ctx, server, r.run, init)
	if err != nil {
		_ = server.Close()
	}
	serveErr := <-done
	if err != nil || serveErr != nil {
		return errors.New("operator-session-incomplete")
	}
	if final == nil || final.ScenarioDigest != init.Scenario.Digest || final.Latched != r.session.Latched() {
		return errors.New("operator-session-incomplete")
	}
	return nil
}
