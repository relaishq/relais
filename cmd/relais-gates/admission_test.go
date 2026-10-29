package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

type admissionServer struct {
	fixture               resourceFixture
	mode                  string
	manifests             map[operatorcaller.Phase]operatorcaller.Manifest
	feeds                 map[string]operatorcaller.Feed
	active                operatorcaller.Phase
	starts, begins, stops []operatorcaller.Phase
	completed, idle       bool
	requests              int
	seen                  map[string]bool
}

func (a *admissionServer) sessions() []string {
	if a.active == operatorcaller.Survivor {
		return []string{"survivor"}
	}
	if a.active == operatorcaller.AdmissionBoundary {
		return []string{"boundary-one", "boundary-two"}
	}
	return nil
}
func (a *admissionServer) status() controlplane.Status {
	s := controlplane.Status{Ready: true, Calls: []controlplane.CallStatus{}, Workers: []controlplane.WorkerStatus{}, Takeovers: []controlplane.MoveResult{}}
	s.Pool.Cap = 2
	for _, worker := range []string{"w1", "w2"} {
		s.Workers = append(s.Workers, controlplane.WorkerStatus{Name: worker, Address: a.fixture.Config.Parameters.Workers[worker].Media})
	}
	for _, id := range a.sessions() {
		s.Calls = append(s.Calls, controlplane.CallStatus{ID: id, Owner: "w1", Epoch: 1})
	}
	return s
}
func (a *admissionServer) reply(t *testing.T, q operator.Request) operator.Reply {
	t.Helper()
	a.requests++
	if a.seen["unknown-refusal"] {
		t.Fatal("MUTATION: optional-session effect after unknown refusal", q.Operation, q.Action)
	}
	if q.ID != strconv.Itoa(a.requests) || q.RemainingMS <= 0 || q.RemainingMS > operatorgates.GateMaximum.Milliseconds() {
		t.Fatal("MUTATION: optional-session request bounds", q.ID)
	}
	reply := operator.Reply{ID: q.ID, Transport: operator.Succeeded, ExitCode: new(int)}
	payload := any(nil)
	switch q.Operation {
	case operator.HostExecute:
		if (a.mode == "unknown-refusal" || a.mode == "unknown-exit-refusal") && a.active != "" && q.Action == "status" {
			a.seen["unknown-refusal"] = true
			reply.Transport = operator.Unknown
			if a.mode == "unknown-exit-refusal" {
				reply.Transport = operator.Succeeded
				reply.ExitCode = nil
			}
			reply.Payload = packedHost(t, []byte(`{"kind":"refused"}`))
			return reply
		}
		if q.Action == "inventory" {
			reply.Payload = packedHost(t, a.fixture.Inventories[q.Target])
			return reply
		}
		if q.Action == "clock" {
			reply.Payload = packedHost(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"`+strconv.FormatInt(time.Now().UnixNano(), 10)+`"}`))
			return reply
		}
		switch q.Action {
		case "status":
			if a.completed && a.active == "" {
				a.seen["idle-status"] = true
			}
			payload = a.status()
		case "telemetry":
			payload = operatorgates.HostTelemetry{Metrics: []string{}, Ages: []mediaworker.AgentSessionStatus{}}
		case "store-status":
			if a.active != "" {
				t.Fatal("MUTATION: optional-session idle checked before survivor stop")
			}
			if !a.seen["idle-status"] || !a.seen["idle/w1"] || !a.seen["idle/w2"] {
				t.Fatal("MUTATION: optional-session incomplete idle proof")
			}
			a.idle = true
			payload = map[string]any{"owners": map[string]int{"w1": 0, "w2": 0}, "transfers": 0}
		case "worker-status":
			match := regexp.MustCompile(`p=json.loads\(base64.b64decode\('([^']+)'\)\)`).FindSubmatch(q.Script)
			if len(match) != 2 {
				t.Fatal("worker parameters missing")
			}
			raw, _ := base64.StdEncoding.DecodeString(string(match[1]))
			var params struct{ Worker string }
			if json.Unmarshal(raw, &params) != nil {
				t.Fatal("worker parameters invalid")
			}
			local := mediaworker.WorkerStatus{Address: a.fixture.Config.Parameters.Workers[params.Worker].Media, InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}
			if params.Worker == "w1" {
				for _, id := range a.sessions() {
					local.Inventory = append(local.Inventory, mediaworker.InstalledSession{ID: id, Epoch: 1, Owner: local.Address})
				}
			}
			if a.active != "" {
				a.seen[string(a.active)+"/"+params.Worker] = true
			} else if a.completed {
				a.seen["idle/"+params.Worker] = true
			}
			local.Sessions = len(local.Inventory)
			payload = local
		default:
			t.Fatal("unexpected admission host effect", q.Action)
		}
		data, _ := json.Marshal(map[string]any{"kind": "ok", "started": time.Now(), "ended": time.Now(), "data": payload})
		reply.Payload = packedHost(t, data)
		return reply
	case operator.CallerReady, operator.CallerStart, operator.CallerStop:
		var args operatorgates.CallerRequest
		if json.Unmarshal(q.Args, &args) != nil || args.Manifest.Validate(args.Manifest.Identity, time.Now()) != nil {
			t.Fatal("invalid admission manifest")
		}
		phase := args.Manifest.Phase
		a.manifests[phase] = args.Manifest
		switch q.Operation {
		case operator.CallerReady:
			payload = operatorgates.CallerProof{Ready: true}
		case operator.CallerStart:
			a.starts = append(a.starts, phase)
			payload = operatorgates.CallerProof{Start: operatorcaller.Started}
			if phase == operatorcaller.AdmissionBoundary && a.mode == "unknown-start" {
				reply.Transport = operator.Unknown
			}
		case operator.CallerStop:
			a.stops = append(a.stops, phase)
			if phase == operatorcaller.Survivor && !a.completed {
				t.Fatal("MUTATION: optional-session stop before terminal evidence")
			}
			if phase == operatorcaller.Survivor && a.mode == "unknown-stop" {
				reply.Transport = operator.Unknown
			} else {
				a.active = ""
			}
			payload = operatorgates.CallerProof{Inactive: reply.Transport == operator.Succeeded}
		}
	case operator.MailboxCreate, operator.MailboxRead:
		var args operatorcaller.MailboxArgs
		if json.Unmarshal(q.Args, &args) != nil {
			t.Fatal("invalid mailbox request")
		}
		var m operatorcaller.Manifest
		for _, candidate := range a.manifests {
			if strings.HasPrefix(args.Key, candidate.Root()+"/") || strings.HasPrefix(args.Key, candidate.Job()+"/") {
				m = candidate
				break
			}
		}
		if m.RunID == "" {
			t.Fatal("unowned mailbox key")
		}
		if q.Operation == operator.MailboxCreate {
			if strings.Contains(args.Key, "/feed/") {
				var feed operatorcaller.Feed
				if json.Unmarshal(args.Data, &feed) != nil {
					t.Fatal("invalid feed")
				}
				if feed.Payload.Kind == "begin" {
					if m.Phase == operatorcaller.AdmissionBoundary && (!a.completed || !a.idle || !reflect.DeepEqual(a.stops, []operatorcaller.Phase{operatorcaller.Survivor})) {
						t.Fatal("MUTATION: optional-session boundary before terminal/stop/idle")
					}
					a.active = m.Phase
					a.begins = append(a.begins, m.Phase)
				} else if feed.Sequence >= 0 {
					a.feeds[m.Job()+"/acks/"+fmt.Sprintf("%05d", feed.Sequence)+".json"] = feed
				}
			}
			payload = operatorcaller.MailboxValue{State: operatorcaller.Committed}
		} else {
			now := time.Now().Add(-time.Millisecond)
			var value any
			switch {
			case strings.HasSuffix(args.Key, "/setup-ready.json"):
				value = operatorcaller.Setup{SchemaVersion: 1, RunID: m.RunID, PublicSHA: m.PublicSHA, Job: "caller-1", CallerID: m.CallerID, Attempt: m.Attempt, RuleAddedAt: now, SetupReadyAt: now, SignalRTTsNS: []int64{1}}
			case strings.HasSuffix(args.Key, "/ready.json"):
				sessions := []string{"survivor"}
				if m.Phase == operatorcaller.AdmissionBoundary {
					sessions = []string{"boundary-one", "boundary-two"}
				}
				value = operatorcaller.Ready{SchemaVersion: 1, RunID: m.RunID, PublicSHA: m.PublicSHA, Job: "caller-1", CallerID: m.CallerID, Attempt: m.Attempt, Sessions: sessions, PID: 42, ReadyAt: now, Host: operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "fixture", CallerGOMAXPROCS: 2, Arch: "arm64", Platform: "linux", CapturedAt: now}}
			case strings.Contains(args.Key, "/acks/"):
				feed, ok := a.feeds[args.Key]
				if !ok {
					t.Fatal("missing feed reservation")
				}
				burst := operatorcaller.Burst{StartedAt: now.Add(-time.Millisecond), FinishedAt: now}
				for range operatorcaller.ClockBurstCount {
					burst.Probes = append(burst.Probes, operatorcaller.Probe{RTTNS: 1, CallerSend: burst.StartedAt, CallerReceive: now, ServerReceive: burst.StartedAt, ServerSend: now})
				}
				probes, _ := json.Marshal(burst)
				value = operatorcaller.Ack{RunID: m.RunID, Job: "caller-1", Sequence: feed.Sequence, OperationID: feed.OperationID, OK: true, ClockProbes: probes}
			case strings.HasSuffix(args.Key, "/finished.json"):
				if m.Phase == operatorcaller.Survivor {
					a.completed = true
				}
				value = map[string]any{"runId": m.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": a.sessions()}
			case strings.HasSuffix(args.Key, "/caller.jsonl"):
				var rows strings.Builder
				for _, id := range a.sessions() {
					restarts := 0
					if a.mode == "known-failure" && m.Phase == operatorcaller.AdmissionBoundary {
						restarts = 1
					}
					row, _ := json.Marshal(map[string]any{"type": "call", "pid": 42, "session": id, "call": map[string]any{"HungUpAt": 1, "DecryptionFailures": map[string]int{"AuthTag": 0, "Replay": 0, "Other": 0}, "ICERestarts": restarts}})
					rows.Write(row)
					rows.WriteByte('\n')
				}
				value = rows.String()
			default:
				t.Fatal("unexpected mailbox read", args.Key)
			}
			data, _ := json.Marshal(value)
			if raw, ok := value.(string); ok {
				data = []byte(raw)
			}
			found := true
			payload = operatorcaller.MailboxValue{Found: &found, Data: data}
		}
	default:
		t.Fatal("unexpected optional operation", q.Operation)
	}
	reply.Payload, _ = json.Marshal(payload)
	return reply
}
func TestSessionBinaryOwnsAdmission(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "relais-gates")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, mode := range []string{"offered", "absent", "invalid-plan", "unknown-start", "unknown-stop", "unknown-refusal", "unknown-exit-refusal", "known-failure", "store-absent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fixture := sessionFixture(t)
			summary, _ := json.Marshal(operatorevidence.Summary{Status: operator.Inconclusive})
			script := []byte("#!/bin/sh\nprintf '%s\\n' '" + string(summary) + "'\n")
			if err := os.WriteFile(fixture.Config.JudgeBinary, script, 0700); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(script)
			fixture.Scenario.Judge.SHA256 = hex.EncodeToString(sum[:])
			fixture.Scenario.Gates = []operator.Gate{operator.Load}
			if mode == "store-absent" {
				fixture.Scenario.Gates = []operator.Gate{operator.Failover}
			}
			fixture.Config.Admission = &operatorgates.AdmissionPlan{SurvivorSeconds: 1, BoundarySeconds: 1}
			if mode == "invalid-plan" {
				fixture.Config.Admission.BoundarySeconds = 1200
			}
			config := filepath.Join(t.TempDir(), "config.json")
			raw, _ := json.Marshal(fixture.Config)
			if err := os.WriteFile(config, raw, 0600); err != nil {
				t.Fatal(err)
			}
			peer, err := operatortransport.Start(ctx, binary, []string{"session", "-config", config})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			if f, err := peer.Receive(ctx); err != nil || f.Type != "hello" {
				t.Fatal(f, err)
			}
			now := time.Now()
			init := operator.Init{Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute, operator.CallerReady, operator.CallerStart, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}}
			if mode != "absent" && mode != "store-absent" {
				init.Capabilities = append(init.Capabilities, fixture.OptionalCapabilities[operator.Load]...)
			}
			if err = peer.Send(ctx, operator.Frame{Type: "init", Session: "run-resources", Init: &init}); err != nil {
				t.Fatal(err)
			}
			server := admissionServer{fixture: fixture, mode: mode, manifests: map[operatorcaller.Phase]operatorcaller.Manifest{}, feeds: map[string]operatorcaller.Feed{}, seen: map[string]bool{}}
			var final *operator.Result
			for final == nil {
				f, err := peer.Receive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch f.Type {
				case "request":
					reply := server.reply(t, *f.Request)
					if err = peer.Send(ctx, operator.Frame{Type: "result", Session: "run-resources", Result: &reply}); err != nil {
						t.Fatal(err)
					}
				case "final":
					final = f.Final
				}
			}
			_ = peer.Close()
			if err = peer.Wait(); err != nil {
				t.Fatal(err)
			}
			if final.Status == operator.Passed || final.Complete {
				t.Fatal("MUTATION: optional-session fabricated media pass", final)
			}
			if mode == "absent" || mode == "store-absent" || mode == "invalid-plan" {
				if server.requests != 0 || final.Latched || final.Gates[0].Reason != "precondition-failed" {
					t.Fatal("MUTATION: optional-session missing capability", final, server.requests)
				}
				return
			}
			if !reflect.DeepEqual(server.starts, []operatorcaller.Phase{operatorcaller.Survivor, operatorcaller.AdmissionBoundary}) {
				t.Fatal("MUTATION: optional-session missing phase", server.starts)
			}
			if strings.HasPrefix(mode, "unknown") {
				if !final.Latched || final.Status != operator.Inconclusive || len(server.begins) > 1 || len(final.Uncertainties) == 0 {
					t.Fatal("MUTATION: optional-session unknown not fenced", final, server.begins)
				}
				if strings.HasSuffix(mode, "refusal") && (!server.seen["unknown-refusal"] || final.Gates[0].Status != operator.Inconclusive) {
					t.Fatal("MUTATION: optional-session unknown refusal became definite", final)
				}
			} else {
				if !reflect.DeepEqual(server.begins, server.starts) || !reflect.DeepEqual(server.stops, server.starts) {
					t.Fatal("MUTATION: optional-session incomplete lifecycle", server.begins, server.stops, final)
				}
				for _, phase := range server.starts {
					for _, worker := range []string{"w1", "w2"} {
						if !server.seen[string(phase)+"/"+worker] {
							t.Fatal("MUTATION: optional-session skipped measurement definition", phase, worker)
						}
					}
				}
				if (mode == "known-failure") != (final.Status == operator.Failure) {
					t.Fatal("MUTATION: optional-session lost failure", final)
				}
			}
		})
	}
}
