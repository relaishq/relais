package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

func TestRehearsalUsesSessionResourceRequests(t *testing.T) {
	raw, err := os.ReadFile("../operatorgates/testdata/resource-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Scenario operator.Scenario
		Config   operatorgates.ResourceConfig
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	binary := filepath.Join(work, "judge")
	if err = os.WriteFile(binary, []byte("judge-stub"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := commandRunner(func(_ context.Context, c Command) CommandResult {
		calls++
		if c.Label != "host-step" || len(c.Input) == 0 {
			t.Fatal("unexpected resource effect", c.Label)
		}
		return CommandResult{Err: context.DeadlineExceeded}
	})
	topology := fixtureTopology(t)
	r := &rehearsal{work: work, run: "run-resources", runner: runner, diagnostics: io.Discard, topology: topology, pins: fixture.Config.Pins, report: &Report{Bounds: fixture.Config.Bounds}, judge: &recordingJudge{pinned: operatorevidence.PinnedJudge{Binary: binary, SHA256: fixture.Scenario.Judge.SHA256}}}
	// The public fixture's architecture is arm64. The rehearsal is x86_64;
	// these negative tests stop before decoding any live manifest.
	r.deployment = &Deployment{Topology: topology, containerIDs: fixtureContainerIDs()}
	r.report.Bounds = clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100}
	now := time.Now()
	init := operator.Init{Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = r.runResourceSequence(ctx, init); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || r.session == nil || !r.session.Latched() || len(r.report.Gates) != 1 || r.report.Gates[0].Reason != "command-result-unknown" || r.report.Gates[0].ProcedureCompleted {
		t.Fatalf("MUTATION: rehearsal-session-seam calls=%d gates=%+v", calls, r.report.Gates)
	}
}
func TestMailboxResourceServerPreservesCreateAndBlobEvidence(t *testing.T) {
	dir := t.TempDir()
	store, err := operatorcaller.OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	spool, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	resource := mailboxResources{store: store, spool: spool}
	data := []byte(strings.Repeat("e", 300000))
	makeReq := func(id string, op operator.Operation, args operatorcaller.MailboxArgs) operator.Request {
		raw, _ := json.Marshal(args)
		return operator.Request{ID: id, Operation: op, Args: raw}
	}
	// Large values arrive from the independent caller process, not a conductor
	// create frame. Read uses the already-defined top-level Reply.Blob envelope.
	if got := store.Create(context.Background(), "caller/report", data); got != operatorcaller.Committed {
		t.Fatal(got)
	}
	reply := resource.Read(context.Background(), makeReq("1", operator.MailboxRead, operatorcaller.MailboxArgs{Key: "caller/report"}))
	if reply.Blob == nil || len(reply.Payload) != 0 {
		t.Fatal("large evidence bypassed blob spool")
	}
	resolved, err := operatortransport.ResolveReply(spool, reply)
	var value operatorcaller.MailboxValue
	if err != nil || json.Unmarshal(resolved.Payload, &value) != nil || value.Found == nil || !*value.Found || !bytes.Equal(value.Data, data) {
		t.Fatal("blob evidence changed", err)
	}
	for i, want := range []operatorcaller.CreateState{operatorcaller.Committed, operatorcaller.Conflict} {
		reply = resource.Create(context.Background(), makeReq(strconv.Itoa(i+2), operator.MailboxCreate, operatorcaller.MailboxArgs{Key: "caller/feed", Data: []byte("entry")}))
		value = operatorcaller.MailboxValue{}
		if json.Unmarshal(reply.Payload, &value) != nil || value.State != want {
			t.Fatal("create-only proof", reply)
		}
	}
}

func TestRehearsalOffersNoOptionalCapabilities(t *testing.T) {
	adapter := Adapter{Caller: &callerResources{}, Mailbox: mailboxResources{}}
	for _, capability := range adapter.Capabilities() {
		if capability == operator.CallerAdmission || capability == operator.StoreObserve || capability == operator.StoreFailover {
			t.Fatal("MUTATION: rehearsal-optional-capability", capability)
		}
	}
}
