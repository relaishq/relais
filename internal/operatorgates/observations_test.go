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
	}
	if !reflect.DeepEqual(calls, append(append([]string{}, inventory.Slots...), inventory.Slots...)) {
		t.Fatalf("inventory cached or host omitted: %v", calls)
	}
	if _, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, "different", true); err == nil {
		t.Fatal("skip-deployed-release-check: wrong deployment accepted")
	}
	pins[1].Commit = strings.Repeat("f", 40)
	before := len(calls)
	if _, err := session.FreshInventory(context.Background(), peer, pins, time.Minute, "", true); err == nil || len(calls) != before {
		t.Fatal("skip-runtime-release-pins: wrong judge pin read hosts")
	}
}

func TestRuntimeClockCacheOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := loadProcedureFixture(corpus, "e91020ea28c0bc03.json")
	if err != nil {
		t.Fatal(err)
	}
	session, clock := fixtureSession(t)
	runtime := &ProcessRuntime{Session: session}
	roots := procedureRoots(fixture, "processes.ProcessRuntime.verifyClocks")
	if len(roots) != 4 {
		t.Fatal("clock cache invocations changed")
	}
	for _, root := range roots {
		outputs := map[string]json.RawMessage{}
		hosts := map[string]string{}
		expectedReads := 0
		for _, e := range root.events {
			if e.Kind == "request" && e.Operation == "clocks.previewClocks" {
				var bounds clocks.Bounds
				if err := json.Unmarshal(e.Inputs[1], &hosts); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(e.Inputs[2], &bounds); err != nil {
					t.Fatal(err)
				}
				runtime.Bounds = &bounds
			}
			if e.Kind == "result" && strings.HasSuffix(e.Operation, ".now") {
				var now int64
				if json.Unmarshal(e.Value, &now) == nil {
					clock.Now = time.UnixMilli(now)
					clock.Tick = time.Duration(now-1000000) * time.Millisecond
				}
			}
			if e.Kind == "result" && strings.HasSuffix(e.Operation, ".send") {
				var out struct {
					Instance string `json:"InstanceId"`
					Stdout   string `json:"StandardOutputContent"`
				}
				if json.Unmarshal(e.Value, &out) == nil && out.Stdout != "" {
					var envelope struct{ Value json.RawMessage }
					if err := json.Unmarshal([]byte(out.Stdout), &envelope); err != nil {
						t.Fatal(err)
					}
					outputs[out.Instance] = envelope.Value
					expectedReads++
				}
			}
		}
		if root.req.ID == 99 {
			runtime.Bounds = nil
		}
		peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
			if req.Action != "clock" {
				t.Fatal("unexpected clock action")
			}
			data, ok := outputs[hosts[req.Target]]
			if !ok {
				t.Fatal("MUTATION: clock-cache unexpected host read")
			}
			return hostReply(t, data)
		}}
		runtime.Peer = peer
		var deadline int64
		_ = json.Unmarshal(root.req.Inputs[0], &deadline)
		err := runtime.VerifyClocks(context.Background(), time.Duration(deadline-1000000)*time.Millisecond)
		if (err == nil) != (root.res.Completion == "returned") || peer.sent != expectedReads {
			t.Fatal("MUTATION: clock-cache replay", root.req.ID, err, peer.sent, expectedReads)
		}
		if err == nil {
			var want struct {
				At              int64
				Control, Worker int64
				Drift           float64
			}
			if err := json.Unmarshal(root.res.Value, &want); err != nil {
				t.Fatal(err)
			}
			got := runtime.ClockSamples[len(runtime.ClockSamples)-1]
			if got.At.UnixMilli() != want.At || got.Control != want.Control || got.Worker != want.Worker || got.Drift != want.Drift {
				t.Fatal("MUTATION: clock-cache values", root.req.ID, got, want)
			}
		}
	}
	if len(runtime.ClockSamples) != 2 {
		t.Fatal("MUTATION: clock-cache refresh")
	}
}
