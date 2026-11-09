package operatorgates

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/operator"
)

type observationPeer struct {
	respond func(operator.Request) operator.Reply
	request operator.Request
	sent    int
}

func TestObservationCoverageInventory(t *testing.T) {
	counts := map[string]map[string]int{}
	add := func(source, operation string) {
		if counts[source] == nil {
			counts[source] = map[string]int{}
		}
		counts[source][operation]++
	}
	for _, operation := range []string{"inventory.parseInventory", "clocks.previewClocks", "clocks.verifySignalBracket"} {
		for _, c := range oracletest.Cases(t, operation) {
			add(c.Source, operation)
		}
	}
	for _, c := range oracletest.Typed(t) {
		add(c.Source, "observe.typedLost")
	}
	data, err := os.ReadFile("testdata/coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		ObservationReplay struct {
			FunctionInvocations, TypedObservations, Scenarios int
			Evidence                                          string
		} `json:"observationReplay"`
		Fixtures []struct {
			Fixture, FullReplay  string
			ReplayTests          []string          `json:"replayTests"`
			ProcedureProjections []json.RawMessage `json:"procedureProjections"`
			Projections          []struct {
				Operation, Evidence, Owner, Test string
				Invocations                      int
			} `json:"observationProjections"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.ObservationReplay.FunctionInvocations != 179 || inventory.ObservationReplay.TypedObservations != 13 || inventory.ObservationReplay.Scenarios != len(counts) || inventory.ObservationReplay.Evidence != "replayed" {
		t.Fatal("observation replay counts differ")
	}
	for _, f := range inventory.Fixtures {
		if f.FullReplay == "replayed" && len(f.ProcedureProjections) == 0 && len(f.ReplayTests) == 0 && (len(f.Projections) != 1 || f.Projections[0].Operation != "clocks.verifySignalBracket") {
			t.Fatal("outer fixture incorrectly marked replayed", f.Fixture)
		}
		for _, projection := range f.Projections {
			if projection.Owner != "P2" || projection.Evidence != "replayed" || projection.Test == "" || counts[f.Fixture][projection.Operation] != projection.Invocations {
				t.Fatal("invalid observation projection", f.Fixture)
			}
			delete(counts[f.Fixture], projection.Operation)
		}
		if len(counts[f.Fixture]) == 0 {
			delete(counts, f.Fixture)
		}
	}
	if len(counts) != 0 {
		t.Fatal("uninventoried observation replay")
	}
}

func (p *observationPeer) Send(_ context.Context, frame operator.Frame) error {
	p.sent++
	p.request = *frame.Request
	return nil
}
func (p *observationPeer) Receive(context.Context) (operator.Frame, error) {
	r := p.respond(p.request)
	r.ID = p.request.ID
	return operator.Frame{Type: "result", Session: "session", Result: &r}, nil
}
func (p *observationPeer) Poll() (operator.Frame, error, bool) { return operator.Frame{}, nil, false }
func (p *observationPeer) Close() error                        { return nil }
func (p *observationPeer) Finish(ctx context.Context, build func(error) operator.Frame) error {
	return nil
}
func hostReply(t *testing.T, data []byte) operator.Reply {
	var shape map[string]json.RawMessage
	if json.Unmarshal(data, &shape) == nil && len(shape["trackingLog"]) > 0 {
		var log clocks.TrackingLog
		if json.Unmarshal(shape["trackingLog"], &log) == nil {
			testPrecision(log.Rows)
			shape["trackingLog"], _ = json.Marshal(log)
			data, _ = json.Marshal(shape)
		}
	}

	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	_, _ = z.Write(data)
	_ = z.Close()
	output := operator.HostOutput{Stdout: base64.StdEncoding.EncodeToString(b.Bytes())}
	return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: oracletest.Marshal(t, output)}
}

func TestFreshObservationSessionOutcomes(t *testing.T) {
	for _, mode := range []string{"unknown", "late", "failed-read", "malformed", "expired"} {
		t.Run(mode, func(t *testing.T) {
			s, c := fixtureSession(t)
			if mode == "expired" {
				c.Advance(time.Minute)
			}
			peer := &observationPeer{respond: func(request operator.Request) operator.Reply {
				r := hostReply(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"1792886400000000000"}`))
				switch mode {
				case "unknown":
					r.Transport = operator.Unknown
				case "late":
					c.Advance(time.Minute)
				case "failed-read":
					r.Transport = operator.Failed
					exit := 3
					r.ExitCode = &exit
					r.Payload = oracletest.Marshal(t, operator.HostOutput{Stdout: "CLOCK_UNKNOWN"})
				case "malformed":
					r.Payload = json.RawMessage(`{}`)
				}
				return r
			}}
			_, err := s.FreshClocks(context.Background(), peer, clocks.Bounds{}, time.Minute)
			if err == nil {
				t.Fatal("missing observation became clock proof")
			}
			if mode == "expired" {
				if peer.sent != 0 {
					t.Fatal("expired observation sent")
				}
				return
			}
			if peer.sent != 1 {
				t.Fatalf("observation was retried or followed: %d", peer.sent)
			}
			if mode == "failed-read" {
				if s.Latched() {
					t.Fatal("pure failed read became unknown")
				}
				return
			}
			if !s.Latched() || err.Error() != "command-result-unknown" {
				t.Fatalf("unknown observation lost session latch: %v", err)
			}
			if _, err := s.Issue(operator.HostExecute, "shared", "move", nil, nil, 2*time.Minute); err == nil {
				t.Fatal("unknown observation allowed an effect")
			}
		})
	}
}

func TestClockOracle(t *testing.T) {
	cases := oracletest.Cases(t, "clocks.previewClocks")
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			hosts := oracletest.JSON[map[string]string](t, c.Request.Inputs[1])
			bounds := oracletest.JSON[clocks.Bounds](t, c.Request.Inputs[2])
			outputs := map[string][]byte{}
			for _, event := range c.HostResults {
				var response struct {
					Instance string `json:"InstanceId"`
					Stdout   string `json:"StandardOutputContent"`
				}
				_ = json.Unmarshal(event.Value, &response)
				var envelope struct {
					Value json.RawMessage `json:"value"`
				}
				_ = json.Unmarshal([]byte(response.Stdout), &envelope)
				outputs[response.Instance] = envelope.Value
			}
			session, _ := fixtureSession(t)
			// The historical clock oracle assumes signal on relay and control on shared.
			session.inventoryComponents = map[string][]string{"relay": {"signal"}, "shared": {"control"}}
			peer := &observationPeer{respond: func(request operator.Request) operator.Reply {
				data, ok := outputs[hosts[request.Target]]
				if !ok {
					t.Fatal("missing oracle host")
				}
				return hostReply(t, data)
			}}
			got, err := session.FreshClocks(context.Background(), peer, bounds, time.Minute)

			if (err == nil) != (c.Result.Completion == "returned") {
				t.Fatalf("clock oracle verdict: %s: %v", c.Scenario, err)
			}
			if err == nil {
				var expected struct {
					Control, Worker int64
					Drift           float64
				}
				_ = json.Unmarshal(c.Result.Value, &expected)
				if got.Control != expected.Control || got.Worker != expected.Worker || got.Drift != expected.Drift {
					t.Fatalf("clock oracle values differ: %+v", got)
				}
			}
		})
	}
	t.Logf("replayed %d clock invocations", len(cases))
}

func TestFreshInventoryReadsAllHostsAndChecksDeployment(t *testing.T) {
	fixtures := map[string]json.RawMessage{}
	var pins []inventory.Pin
	for _, c := range oracletest.Cases(t, "inventory.parseInventory") {
		if c.Result.Completion != "returned" {
			continue
		}
		slot := oracletest.JSON[string](t, c.Request.Inputs[0])
		var encoded struct {
			Value json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal([]byte(oracletest.JSON[string](t, c.Request.Inputs[1])), &encoded)
		if _, ok := fixtures[slot]; !ok {
			fixtures[slot] = oracletest.ProductJSON(t, encoded.Value)
			pins = oracletest.JSON[[]inventory.Pin](t, c.Request.Inputs[2])
		}
	}
	var calls []string
	selected := operator.Scenario{}
	for i := range pins {
		pins[i].BootSHA256 = strings.Repeat("d", 64)
		identity := operator.Identity{SourceCommit: pins[i].Commit, SHA256: pins[i].ArchiveSHA256}
		if i == 0 {
			selected.ReleaseA = identity
		} else {
			selected.ReleaseB = identity
		}
	}
	session, clock := fixtureSession(t)
	selected.SchemaVersion = 1
	selected.Digest = strings.Repeat("a", 64)
	selected.Gates = []operator.Gate{operator.Rollout}
	session, err := NewSession(session.ID, operator.Init{Scenario: selected, ArmedAtUnixMS: "1000000", EndUnixMS: "7300000", Capabilities: []operator.Operation{operator.HostExecute}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	// Later changes to the init value cannot select another release for this session.
	selected.ReleaseA.SourceCommit = "changed"
	peer := &observationPeer{respond: func(request operator.Request) operator.Reply {
		calls = append(calls, request.Target)
		if !bytes.Contains(request.Script, []byte("require_running=True")) || request.DeadlineUnixMS == "" {
			t.Fatal("inventory host contract missing")
		}
		return hostReply(t, fixtures[request.Target])
	}}

	for range 2 {
		pool, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, pins[0].Commit, true)
		if err != nil || len(pool.Participants) != 6 {
			t.Fatalf("fresh inventory: %v", err)
		}
		if slot, ok := inventory.ComponentSlot(session.inventoryComponents, "signal"); !ok || slot != "shared" {
			t.Fatal("fresh inventory did not retain the observed signal component", slot, ok)
		}
	}
	if !reflect.DeepEqual(calls, append(append([]string{}, inventory.Slots...), inventory.Slots...)) {
		t.Fatalf("inventory cached or host omitted: %v", calls)
	}
	inventoryReply := peer.respond
	for _, largeSlot := range []string{"shared", "relay"} {
		peer.respond = func(request operator.Request) operator.Reply {
			sample := clocks.Sample{ErrorBoundNS: 1, RateErrorPPM: 1, MeasuredUnixNS: "1793930000000000000"}
			if request.Target == largeSlot {
				sample.ErrorBoundNS, sample.RateErrorPPM = 20, 20
			}
			return hostReply(t, oracletest.Marshal(t, sample))
		}
		e, err := session.FreshClocks(context.Background(), peer, clocks.Bounds{FrontendControlBoundNS: 10, FrontendWorkerBoundNS: 10, DriftPPM: 10}, time.Minute)
		if largeSlot == "shared" {
			if err == nil || err.Error() != "measured clocks exceed approved bounds" || e.Control != 40 || e.Worker != 21 || e.Drift != 40 {
				t.Fatal("fresh inventory did not select shared for signal and control", e, err)
			}
		} else if err != nil || e.Control != 2 || e.Worker != 2 || e.Drift != 2 {
			t.Fatal("relay clock entered shared frontend bounds", e, err)
		}
	}
	peer.respond = inventoryReply
	if _, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, "different", true); err == nil {
		t.Fatal("skip-deployed-release-check: wrong deployment accepted")
	}
	// A definite inventory failure must not reclassify or re-emit prior probes.
	rows := []clocks.TrackingRow{}
	for n := int64(-3); n <= 3; n++ {
		rows = append(rows, clocks.TrackingRow{UnixSeconds: 1793930000 + n, MaxErrorSeconds: 0.0005, LeapStatus: "N"})
	}
	line, err := (clockprobe.Record{Target: clockprobe.WorkerOne, FrontendSend: 1793930000100000000, FrontendReceive: 1793930000102000000, HostReceive: 1793930000111000000, HostSend: 1793930000111000000}).MarshalLine()
	if err != nil {
		t.Fatal(err)
	}
	batch, err := json.Marshal(clocks.ProbeBatch{Lines: string(line), Cursor: json.RawMessage(`{"file":"1:2","position":84,"anchor":"` + strings.Repeat("a", 64) + `","skipLine":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	session.clockProbeHistory.Collect(map[string]clocks.Sample{"shared": {TrackingLog: &clocks.TrackingLog{Rows: rows}}, "w1": {TrackingLog: &clocks.TrackingLog{Rows: rows}}}, "shared", batch)
	snapshot := func() *clocks.Falsification {
		frontend, _ := inventory.ComponentSlot(session.inventoryComponents, "signal")
		return session.clockProbeHistory.Snapshot(frontend, map[string]string{"w1": "w1"})
	}
	if e := snapshot(); e.CountsByStatus["falsified"] != 1 || len(e.Records) != 1 {
		t.Fatal("missing initial falsification", e)
	}
	respond := peer.respond
	for range 2 {
		peer.respond = func(operator.Request) operator.Reply { return hostReply(t, []byte(`{}`)) }
		if _, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, "", true); err == nil {
			t.Fatal("invalid inventory accepted")
		}
		if e := snapshot(); e.CountsByStatus["falsified"] != 1 || len(e.Records) != 0 || e.LossCounts.OmittedRecords != 0 {
			t.Fatal("failed inventory churned probe evidence", e)
		}
		peer.respond = respond
		if _, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, pins[0].Commit, true); err != nil {
			t.Fatal(err)
		}
		if e := snapshot(); e.CountsByStatus["falsified"] != 1 || len(e.Records) != 0 || e.LossCounts.OmittedRecords != 0 {
			t.Fatal("inventory recovery churned probe evidence", e)
		}
	}

	pins[1].Commit = strings.Repeat("f", 40)
	before := len(calls)
	if _, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, "", true); err == nil || len(calls) != before {
		t.Fatal("skip-runtime-release-pins: wrong judge pin read hosts")
	}
}

func TestBoundLeaseCacheAndRefresh(t *testing.T) {
	r, p, _ := runtimeFixture(t)
	c := r.Session.clock.(*FakeClock)
	for i, d := range []time.Duration{0, time.Second, 5 * time.Second} {
		c.Advance(d)
		if err := r.VerifyClocks(context.Background(), c.Elapsed()+time.Minute); err != nil {
			t.Fatal("MUTATION: clock-cache", err)
		}
		want := 4
		if i == 2 {
			want = 8
		}
		if p.sent != want || !r.ClockSamples[len(r.ClockSamples)-1].At.Equal(c.Wall()) {
			t.Fatal("MUTATION: clock-cache", i, p.sent, want)
		}
	}
}

func TestTrackingCursorFollowsEachHostAndBreaksStayRecorded(t *testing.T) {
	s, _ := fixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}}
	round := 0
	peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
		script := string(req.Script)
		if !strings.Contains(script, ", 1000000)") {
			t.Fatal("run start was not sent")
		}
		cursor := `{"position":1}`
		if round > 0 {
			if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(cursor))) {
				t.Fatal("host cursor was not sent", req.Target)
			}
			cursor = `{"position":2}`
		}
		return hostReply(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"1792886400000000000","trackingLog":{"cursor":`+cursor+`,"rows":[],"breaks":[{"kind":"self-consistency","unixSeconds":1792886400}],"runStartUnixMs":1000000}}`))
	}}
	for round = 0; round < 2; round++ {
		e, err := s.FreshClocks(context.Background(), peer, clocks.Bounds{}, time.Minute)
		if err != nil || len(e.Clocks) != 4 {
			t.Fatal("tracking changed clock verdict", err)
		}
		for _, sample := range e.Clocks {
			if sample.TrackingLog == nil || len(sample.TrackingLog.Breaks) != 1 {
				t.Fatal("lost tracking break")
			}
		}
	}
}

func TestFreshClocksRequiresUniqueSignalAndControl(t *testing.T) {
	for _, tc := range []struct {
		name       string
		components map[string][]string
	}{
		{"inventory not read", nil},
		{"missing signal", map[string][]string{"shared": {"control"}}},
		{"missing control", map[string][]string{"shared": {"signal"}}},
		{"duplicate signal", map[string][]string{"shared": {"signal", "control"}, "relay": {"signal"}}},
		{"duplicate control", map[string][]string{"shared": {"signal", "control"}, "relay": {"control"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixtureSession(t)
			s.inventoryComponents = tc.components
			peer := &observationPeer{respond: func(operator.Request) operator.Reply {
				return hostReply(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"1793930000000000000"}`))
			}}
			e, err := s.FreshClocks(context.Background(), peer, clocks.Bounds{}, time.Minute)
			if err == nil || err.Error() != "missing independent clock evidence" || s.Latched() || len(e.Clocks) != 4 || peer.sent != 4 {
				t.Fatal("unknown component slot became clock proof or changed read/latch behavior", e, err)
			}
		})
	}
}

func TestTrackingFailedBatchRefreshIsRecordedAndCountsTowardCap(t *testing.T) {
	s, _ := fixtureSession(t)
	s.capabilities[operator.HostReadBatch] = true
	var output bytes.Buffer
	recorder := &TimingRecorder{Writer: &output}
	peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
		if req.Operation != operator.HostReadBatch {
			t.Fatal("expected batch read")
		}
		var args operator.HostReadBatchArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			t.Fatal(err)
		}
		batch := operator.HostReadBatchOutput{}
		for _, read := range args.Reads {
			child := hostReply(t, []byte(`{"errorBoundNs":0,"rateErrorPpm":0,"measuredUnixNs":"1792886400000000000","trackingLog":{"cursor":{"position":1},"rows":[],"breaks":[]}}`))
			child.ID = read.Target
			if read.Target == "shared" {
				child.ExitCode = new(1)
				child.Payload = oracleJSON(operator.HostOutput{Stdout: "CLOCK_UNKNOWN"})
			}
			batch.Replies = append(batch.Replies, child)
		}
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(0), Payload: oracleJSON(batch)}
	}}
	e, err := s.FreshClocks(context.Background(), peer, clocks.Bounds{}, time.Minute)
	recorder.RecordClocks(e)
	if err == nil {
		t.Fatal("failed batch became a successful refresh")
	}
	var record struct {
		Evidence clocks.Evidence `json:"evidence"`
	}
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Evidence.At.IsZero() || record.Evidence.Clocks["w1"].TrackingLog == nil || recorder.refreshes != 1 || s.Latched() || peer.sent != 1 {
		t.Fatal("failed refresh lost partial evidence, changed the cap, or changed failure semantics", output.String())
	}
}
