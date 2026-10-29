package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

type resourceFixture struct {
	Scenario             operator.Scenario
	Config               operatorgates.ResourceConfig
	Inventories          map[string]json.RawMessage
	ExpectedUnknownStart []string
	Replies              map[string]json.RawMessage
	OptionalCapabilities map[operator.Gate][]operator.Operation
}

func sessionFixture(t *testing.T) resourceFixture {
	t.Helper()
	raw, err := os.ReadFile("../../internal/operatorgates/testdata/resource-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var f resourceFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	f.Config.JudgeBinary = filepath.Join(t.TempDir(), "judge")
	if err = os.WriteFile(f.Config.JudgeBinary, []byte("judge-stub"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}
func packedHost(t *testing.T, data []byte) json.RawMessage {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(operator.HostOutput{Stdout: base64.StdEncoding.EncodeToString(b.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSessionBinaryUsesResourceSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "relais-gates")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build session: %v %s", err, out)
	}
	for _, mode := range []string{"unknown-start", "unknown-start-evidence", "unknown-create", "not-near", "missing-capability", "definite-refusal", "unknown-inventory"} {
		t.Run(mode, func(t *testing.T) {
			fixture := sessionFixture(t)
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
			hello, err := peer.Receive(ctx)
			if err != nil || hello.Type != "hello" {
				t.Fatal(hello, err)
			}
			now := time.Now()
			init := operator.Init{Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute, operator.CallerReady, operator.CallerStart, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}}
			if mode == "missing-capability" {
				init.Capabilities = []operator.Operation{operator.HostExecute, operator.CallerReady, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}
			}
			if err = peer.Send(ctx, operator.Frame{Type: "init", Session: "run-resources", Init: &init}); err != nil {
				t.Fatal(err)
			}
			requests := []string{}
			traces := []operator.Trace{}
			var final *operator.Result
			var manifest operatorcaller.Manifest
			for final == nil {
				f, err := peer.Receive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch f.Type {
				case "request":
					q := f.Request
					requests = append(requests, string(q.Operation)+"/"+q.Target+"/"+q.Action)
					if q.ID != strconv.Itoa(len(requests)) || q.RemainingMS <= 0 || q.RemainingMS > 15*time.Minute.Milliseconds() || q.DeadlineUnixMS == "" {
						t.Fatal("MUTATION: session-resource-sequence request identity/deadline", q)
					}
					reply := operator.Reply{ID: q.ID, Transport: operator.Succeeded, ExitCode: new(int)}
					switch q.Operation {
					case operator.HostExecute:
						switch q.Action {
						case "inventory":
							reply.Payload = packedHost(t, fixture.Inventories[q.Target])
							if mode == "unknown-inventory" {
								reply.Transport = operator.Unknown
							}
						case "status":
							status := controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{}, Calls: []controlplane.CallStatus{}, Takeovers: []controlplane.MoveResult{}}
							for _, worker := range []string{"w1", "w2"} {
								status.Workers = append(status.Workers, controlplane.WorkerStatus{Name: worker, Address: fixture.Config.Parameters.Workers[worker].Media})
							}
							status.Pool.Cap = 2
							envelope, _ := json.Marshal(map[string]any{"kind": "ok", "started": time.Now(), "ended": time.Now(), "data": status})
							reply.Payload = packedHost(t, envelope)
						default:
							t.Fatal("unexpected host action", q.Action)
						}
					case operator.CallerReady:
						var args operatorgates.CallerRequest
						if err := json.Unmarshal(q.Args, &args); err != nil || args.Manifest.Validate(args.Manifest.Identity, time.Now()) != nil {
							t.Fatal("invalid public manifest", err)
						}
						manifest = args.Manifest
						reply.Payload = fixture.Replies["caller.ready"]
					case operator.CallerStart:
						reply.Transport = operator.Unknown
						reply.Payload = fixture.Replies["caller.start"]
						if mode == "unknown-start-evidence" {
							reply.Transport = operator.Succeeded
							reply.Payload = fixture.Replies["caller.start.unknown"]
						}
						if mode == "not-near" || mode == "unknown-create" {
							reply.Transport = operator.Succeeded
						}
						if mode == "definite-refusal" {
							reply = operator.Reply{ID: q.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
						}
					case operator.MailboxCreate:
						var args operatorcaller.MailboxArgs
						if json.Unmarshal(q.Args, &args) != nil || !strings.HasPrefix(args.Key, manifest.Root()+"/") || len(args.Data) == 0 {
							t.Fatal("invalid public mailbox create")
						}
						reply.Payload = fixture.Replies["mailbox.create"]
						if mode == "unknown-create" {
							reply.Transport = operator.Unknown
						}
					case operator.MailboxRead:
						setup := operatorcaller.Setup{SchemaVersion: 1, RunID: manifest.RunID, PublicSHA: manifest.PublicSHA, Job: "caller-1", CallerID: manifest.CallerID, Attempt: manifest.Attempt, RuleAddedAt: time.Now(), SetupReadyAt: time.Now(), SignalRTTsNS: []int64{int64(20 * time.Millisecond)}}
						data, _ := json.Marshal(setup)
						found := true
						reply.Payload, _ = json.Marshal(operatorcaller.MailboxValue{Found: &found, Data: data})
					case operator.CallerStop:
						reply.Payload = fixture.Replies["caller.stop"]
					default:
						// Keep observing a bad mutant's requests so the named assertion, rather
						// than a broken pipe or timeout, kills it.
						reply.Transport = operator.Unknown
					}
					if err = peer.Send(ctx, operator.Frame{Type: "result", Session: "run-resources", Result: &reply}); err != nil {
						t.Fatal(err)
					}
				case "trace":
					traces = append(traces, *f.Trace)
				case "final":
					final = f.Final
				default:
					t.Fatal("unexpected frame", f.Type)
				}
			}
			_ = peer.Close()
			if err = peer.Wait(); err != nil {
				t.Fatal(err)
			}
			expected := append([]string{}, fixture.ExpectedUnknownStart...)
			if mode == "unknown-create" {
				expected = append(expected, "mailbox.create/caller/mailbox.create")
			}
			if mode == "not-near" {
				expected = append(expected, "mailbox.create/caller/mailbox.create", "mailbox.create/caller/mailbox.create", "mailbox.read/caller/mailbox.read", "caller.stop/caller/caller.stop")
			}
			if mode == "missing-capability" {
				expected = []string{}
			}
			if mode == "unknown-inventory" {
				expected = expected[:1]
			}
			if !reflect.DeepEqual(requests, expected) {
				t.Fatalf("MUTATION: session-resource-sequence mode=%s requests=%v expected=%v", mode, requests, expected)
			}
			wantLatch := strings.HasPrefix(mode, "unknown")
			if final.Latched != wantLatch || final.Status != operator.Inconclusive || final.Complete || len(final.Gates) != 1 || len(final.Trace) != 0 {
				t.Fatalf("MUTATION: session-resource-sequence mode=%s final=%+v", mode, final)
			}
			if wantLatch && (final.FirstError == nil || final.FirstError.Certainty != operator.Unknown || len(final.Uncertainties) == 0) {
				t.Fatalf("MUTATION: session-resource-sequence unknown became definite: %+v traces=%v", final, traces)
			}
			if mode == "missing-capability" && (len(traces) != 0 || final.Gates[0].Reason != "precondition-failed") {
				t.Fatal("MUTATION: session-resource-sequence fabricated capability", final)
			}
		})
	}
}
