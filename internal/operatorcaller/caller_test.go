package operatorcaller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

type testFence struct {
	mu      sync.Mutex
	latched bool
}

func (f *testFence) Latched() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.latched }
func (f *testFence) Halt(string)   { f.mu.Lock(); defer f.mu.Unlock(); f.latched = true }

type testClock struct{ at time.Time }

func (c *testClock) Now() time.Time { return c.at }
func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.at = c.at.Add(d)
	return ctx.Err()
}

type memoryBox struct {
	data     map[string][]byte
	creates  []string
	result   CreateState
	onRead   func(string)
	onCreate func(string, []byte)
}

func (m *memoryBox) Read(_ context.Context, key string) (ReadResult, error) {
	if m.onRead != nil {
		m.onRead(key)
	}
	b, ok := m.data[key]
	return ReadResult{ok, b}, nil
}
func (m *memoryBox) Create(_ context.Context, key string, b []byte) CreateState {
	m.creates = append(m.creates, key)
	if m.onCreate != nil {
		m.onCreate(key, b)
	}
	if m.result != "" && m.result != Committed {
		return m.result
	}
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	if _, exists := m.data[key]; exists {
		return Conflict
	}
	m.data[key] = append([]byte{}, b...)
	return Committed
}
func put(t *testing.T, m *memoryBox, key string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[key] = b
}
func fixtureManifest() Manifest {
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	i := Identity{"run-fixture", operator.Rollout, Survivor, "1", "1", strings.Repeat("a", 40)}
	return Manifest{1, i, at.UnixMilli(), at.UnixMilli(), at.Add(20 * time.Minute).UnixMilli(), NewPlan(Survivor, 60, i.PublicSHA), Endpoints{RunID: i.RunID, Signaling: "https://signal.example.test/calls", Relay: "192.0.2.1:24780", FrontendControlBoundNS: 1000000, FrontendWorkerBoundNS: 1000000, DriftPPM: 10}}
}
func fixtureBurst(at time.Time) Burst {
	var probes []Probe
	for range 10 {
		probes = append(probes, Probe{1000000, at, at, at, at})
	}
	return Burst{at, at, probes}
}
func fixtureHost(at time.Time) Host { return Host{1, 2, 2, "synthetic cpu", 2, "amd64", "linux", at} }
func TestDirectoryAtomicCreateOnly(t *testing.T) {
	d, err := OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	const key = "scope/feed/00000.json"
	var wg sync.WaitGroup
	results := make(chan CreateState, 16)
	for range 16 {
		wg.Go(func() { results <- d.Create(ctx, key, []byte(`{"complete":true}`)) })
	}
	wg.Wait()
	close(results)
	committed := 0
	for result := range results {
		if result == Committed {
			committed++
		} else if result != Conflict {
			t.Fatal(result)
		}
	}
	read, err := d.Read(ctx, key)
	if err != nil || !read.Found || string(read.Data) != `{"complete":true}` || committed != 1 {
		t.Fatal("MUTATION: create-only", read, err, committed)
	}
	for _, key := range []string{"../escape", "/escape", "scope/../escape", "scope\\escape"} {
		if d.Create(ctx, key, []byte("x")) != CreateUnknown {
			t.Fatal("escaped root")
		}
	}
	absent, err := d.Read(ctx, "missing")
	if err != nil || absent.Found {
		t.Fatal(absent, err)
	}
	// A read failure is not absence.
	if err = d.root.Mkdir("directory", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Read(ctx, "directory"); err == nil {
		t.Fatal("directory read became absence")
	}
}
func TestBeginAndHangupOrdering(t *testing.T) {
	m := fixtureManifest()
	ctx := context.Background()
	for _, scenario := range []string{"begin", "feed-first", "terminal", "expired", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			box := &memoryBox{}
			clock := &testClock{time.UnixMilli(m.CreatedAt)}
			begin := Feed{1, m.RunID, -1, "caller-begin", Payload{Kind: "begin", Deadline: time.UnixMilli(m.Deadline)}}
			switch scenario {
			case "begin":
				put(t, box, sequenceKey(m.Root(), -1), begin)
			case "feed-first":
				put(t, box, sequenceKey(m.Root(), 0), Feed{1, m.RunID, 0, "caller-probes", Payload{Kind: "probe"}})
			case "terminal":
				put(t, box, sequenceKey(m.Root(), 0), Feed{1, m.RunID, 0, "caller-stop", Payload{Kind: "hangup"}})
			case "expired":
				begin.Payload.Deadline = clock.at
				put(t, box, sequenceKey(m.Root(), -1), begin)
			case "stopped":
				put(t, box, sequenceKey(m.Root(), -1), begin)
				put(t, box, m.Root()+"/hangup.json", map[string]any{"schemaVersion": 1, "runId": m.RunID})
			}
			setup := Setup{RunID: m.RunID, RuleAddedAt: clock.at}
			err := AwaitBegin(ctx, m, box, clock, setup)
			if (err == nil) != (scenario == "begin") {
				t.Fatal("MUTATION: begin-hangup-barrier", scenario, err)
			}
			if len(box.creates) != 1 || !strings.HasSuffix(box.creates[0], "setup-ready.json") || clock.at.Sub(time.UnixMilli(m.CreatedAt)) < 15*time.Second {
				t.Fatal("barrier order")
			}
		})
	}
	box := &memoryBox{}
	put(t, box, m.Root()+"/hangup.json", map[string]any{"schemaVersion": 1, "runId": m.RunID})
	reads := 0
	box.onRead = func(key string) {
		if strings.HasSuffix(key, "/transport.json") {
			reads++
			if reads > 1 {
				t.Fatal("MUTATION: transport-hangup")
			}
		}
	}
	if _, err := waitManifest(ctx, m.Identity, box, time.Now().Add(time.Second)); err == nil {
		t.Fatal("MUTATION: transport-hangup")
	}
}
func TestBrokerUnknownFencesAndStopCached(t *testing.T) {
	m := fixtureManifest()
	box := &memoryBox{result: CreateUnknown}
	f := &testFence{}
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	b, err := NewBroker(m, NewMailbox(box), clock, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Prepare(context.Background(), strings.Repeat("x", 64), clock.at.Add(time.Minute)); err == nil || !f.Latched() {
		t.Fatal("MUTATION: write-latch")
	}
	n := len(box.creates)
	if n != 1 {
		t.Fatal("MUTATION: broker-create-once")
	}
	_, _ = b.Feed(context.Background(), "operation-next", Payload{Kind: "finish"}, clock.at.Add(time.Minute))
	if len(box.creates) != n {
		t.Fatal("MUTATION: caller-write-fence")
	}
	firstStop := b.Hangup(context.Background(), "caller-stop")
	secondStop := b.Hangup(context.Background(), "caller-stop")
	if firstStop || secondStop || len(box.creates) != n+1 || !strings.HasSuffix(box.creates[n], "/hangup.json") {
		t.Fatal("MUTATION: stop-cache-or-key")
	}
	// Hangup can follow a lost feed and is never queued behind it.
	f = &testFence{}
	box = &memoryBox{result: Conflict}
	if !Hangup(context.Background(), NewMailbox(box), m.Root(), m.RunID, "caller-stop", f) || f.Latched() {
		t.Fatal("MUTATION: own-hangup-conflict")
	}
	box.result = CreateUnknown
	if Hangup(context.Background(), NewMailbox(box), m.Root(), m.RunID, "caller-stop", f) || !f.Latched() {
		t.Fatal("MUTATION: hangup-latch")
	}
}
func TestBrokerRefusesWaitAfterHangup(t *testing.T) {
	m := fixtureManifest()
	box := &memoryBox{}
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	b, _ := NewBroker(m, NewMailbox(box), clock, &testFence{})
	if !b.Hangup(context.Background(), "caller-stop") {
		t.Fatal("hangup")
	}
	before := clock.at
	if _, err := b.Prepare(context.Background(), strings.Repeat("x", 64), clock.at.Add(time.Minute)); err == nil || !clock.at.Equal(before) {
		t.Fatal("MUTATION: stopped-wait")
	}
}
func TestRegistrationBeforeFaultAndExactNearBoundary(t *testing.T) {
	m := fixtureManifest()
	at := time.UnixMilli(m.CreatedAt)
	burst := fixtureBurst(at)
	host := fixtureHost(at)
	for _, ns := range []int64{10000000, 10000001} {
		for i := range burst.Probes {
			burst.Probes[i].RTTNS = ns
		}
		r, err := Register(m.Plan, m.RunID, []string{"one"}, burst, host, at, nil)
		if err != nil || r.Near != (ns == 10000000) {
			t.Fatal("near boundary", r, err)
		}
	}
	if _, err := Register(m.Plan, m.RunID, []string{"one"}, burst, host, at, &at); err == nil {
		t.Fatal("MUTATION: registration-fault-time")
	}
	b, _ := NewBroker(m, NewMailbox(&memoryBox{}), &testClock{at}, &testFence{})
	if err := b.Arm(context.Background(), "drain-w1", []callharness.OperationIdentity{{RunID: m.RunID, OperationID: "drain-w1", SessionID: "one", Worker: "w1", Epoch: 1}}, at.Add(time.Minute)); err == nil {
		t.Fatal("unregistered arm")
	}
}

type execFunc func(context.Context, []byte, time.Time) Execution

func (f execFunc) Execute(ctx context.Context, script []byte, end time.Time) Execution {
	return f(ctx, script, end)
}
func fixtureLaunchConfig() LaunchConfig {
	return LaunchConfig{BridgePath: "/opt/caller/bridge", BridgeSHA256: strings.Repeat("a", 64), LoadPath: "/opt/caller/load", LoadSHA256: strings.Repeat("b", 64), ConfigPath: "/opt/caller/config.json", ConfigSHA256: strings.Repeat("c", 64), RuntimeDeadline: time.Now().Add(19 * time.Minute), MailboxPath: "/opt/caller/mailbox", WorkRoot: "/opt/caller/work", Architecture: "x86_64"}
}
func TestSingleLaunchAndUnknownStop(t *testing.T) {
	i := fixtureManifest().Identity
	f := &testFence{}
	box := &memoryBox{}
	starts, stops, reads := 0, 0, 0
	l := &FixedLauncher{configs: map[Identity]LaunchConfig{i: fixtureLaunchConfig()}, Mailbox: NewMailbox(box), Fence: f}
	l.Exec = execFunc(func(_ context.Context, s []byte, _ time.Time) Execution {
		switch {
		case strings.Contains(string(s), "caller-ready"):
			return Execution{Certainty: operator.Succeeded, Output: "caller-ready\n"}
		case strings.Contains(string(s), "systemd-run"):
			starts++
			return Execution{Certainty: operator.Succeeded, Output: "caller-started:" + i.Unit() + "\n"}
		case strings.Contains(string(s), "systemctl stop"):
			stops++
			return Execution{Certainty: operator.Unknown}
		default:
			reads++
			return Execution{Certainty: operator.Succeeded, Output: "LoadState=loaded\nActiveState=inactive\n"}
		}
	})
	ctx := context.Background()
	end := time.Now().Add(5 * time.Second)
	if l.Ready(ctx, end) != operator.Succeeded || l.Start(ctx, i, i.Phase, end).State != Started {
		t.Fatal("start")
	}
	_ = l.Start(ctx, i, i.Phase, end)
	i.CallerID = "2"
	l.configs[i] = fixtureLaunchConfig()
	_ = l.Start(ctx, i, i.Phase, end)
	i.CallerID = "1"
	if starts != 1 {
		t.Fatal("MUTATION: single-launch", starts)
	}
	result := l.Stop(ctx, i, end)
	again := l.Stop(ctx, i, end)
	if result != again || !result.Inactive || result.Certainty != operator.Unknown || !f.Latched() || stops != 1 || reads != 1 || len(box.creates) != 1 {
		t.Fatal("MUTATION: stop-certainty-cache", result, starts, stops, reads)
	}
	// An unknown launch is never resent, even if the caller asks twice.
	f = &testFence{}
	l = &FixedLauncher{configs: map[Identity]LaunchConfig{i: fixtureLaunchConfig()}, Mailbox: NewMailbox(&memoryBox{}), Fence: f, ready: true}
	starts = 0
	l.Exec = execFunc(func(context.Context, []byte, time.Time) Execution {
		starts++
		return Execution{Certainty: operator.Unknown}
	})
	if l.Start(ctx, i, i.Phase, end).State != StartUnknown || !f.Latched() {
		t.Fatal("MUTATION: start-unknown")
	}
	againStart := l.Start(ctx, i, i.Phase, end)
	if starts != 1 || againStart.State != StartUnknown {
		t.Fatal("MUTATION: start-unknown-resend")
	}
}
func TestInactiveRequiresExactObservation(t *testing.T) {
	for _, x := range []Execution{{Certainty: operator.Unknown, Output: "LoadState=loaded\nActiveState=inactive\n"}, {Certainty: operator.Succeeded}, {Certainty: operator.Succeeded, Output: "ActiveState=inactive\n"}, {Certainty: operator.Succeeded, Output: "LoadState=loaded\nActiveState=active\n"}, {Certainty: operator.Failed, ExitCode: 1, Output: "not found"}, {Certainty: operator.Failed, ExitCode: 1, Output: "LoadState=loaded\nActiveState=inactive\n"}} {
		if inactive(x) {
			t.Fatal("MUTATION: inactive-proof", x)
		}
	}
	if !inactive(Execution{Certainty: operator.Succeeded, Output: "LoadState=not-found\nActiveState=inactive\n"}) || !inactive(Execution{Certainty: operator.Failed, ExitCode: 1, Output: "LoadState=not-found\nActiveState=inactive\n"}) {
		t.Fatal("collected unit")
	}
}
func TestShutdownAndGeneratorAbortBeforeSave(t *testing.T) {
	var mu sync.Mutex
	var signals []os.Signal
	s := NewShutdown(func(signal os.Signal) error { mu.Lock(); signals = append(signals, signal); mu.Unlock(); return nil })
	s.Interrupt(time.Hour)
	s.Interrupt(0)
	s.Exited()
	mu.Lock()
	n := len(signals)
	mu.Unlock()
	if n != 1 {
		t.Fatal("shared grace")
	}
	w := SaturationWindow{PlannedSeconds: 100}
	var order []string
	raw := json.RawMessage(`{"interval_ns":2000000000,"cpu_percent_per_gomaxprocs":0,"interval_lateness":{"p99_ns":0},"interval_pacing":{"sent_frames":100,"skipped_frames":0}}`)
	err := RecordGeneratorSample(raw, &w, func() error { order = append(order, "save"); return errors.New("lost publication") }, func(string) error { order = append(order, "stop"); return nil }, func() { order = append(order, "interrupt") })
	if err == nil || strings.Join(order, ",") != "interrupt,save" {
		t.Fatal("MUTATION: generator-abort-order", order, err)
	}
	if _, err = w.Add(json.RawMessage(`{"interval_ns":1000000000}`)); err == nil {
		t.Fatal("missing generator fields")
	}
}
func TestCredentialFreeEnvironment(t *testing.T) {
	env := PublicEnvironment([]string{"AWS_SECRET_ACCESS_KEY=secret", "RELAIS_TOKEN=secret", "GH_TOKEN=secret", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=secret", "AZURE_CLIENT_SECRET=secret", "GOOGLE_APPLICATION_CREDENTIALS=/secret", "OPENAI_API_KEY=secret", "UNRELATED_PASSWORD=secret", "GOFLAGS=-overlay=/secret", "PATH=/bin", "GOMAXPROCS=8"})
	if strings.Join(env, ",") != "PATH=/bin,GOMAXPROCS=2" {
		t.Fatal("MUTATION: child-environment-allowlist", env)
	}
}
func TestFixedScriptsParse(t *testing.T) {
	i := fixtureManifest().Identity
	c := fixtureLaunchConfig()
	c.WorkRoot = "/opt/caller's work"
	ready, err := ReadyScript(c)
	if err != nil {
		t.Fatal(err)
	}
	start, err := StartScript(c, i, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stop, err := StopScript(i)
	if err != nil {
		t.Fatal(err)
	}
	inactive, err := InactiveScript(i)
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range [][]byte{ready, start, stop, inactive} {
		cmd := exec.Command("/bin/sh", "-n")
		cmd.Stdin = bytes.NewReader(script)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixed script syntax: %v: %s", err, output)
		}
	}
}

type launcherFuncs struct {
	ready func() operator.Certainty
	start func(Identity) StartResult
	stop  func(Identity) StopResult
}

func (l launcherFuncs) Ready(context.Context, time.Time) operator.Certainty { return l.ready() }
func (l launcherFuncs) Start(_ context.Context, i Identity, _ Phase, _ time.Time) StartResult {
	return l.start(i)
}
func (l launcherFuncs) Stop(_ context.Context, i Identity, _ time.Time) StopResult { return l.stop(i) }
func seedBroker(t *testing.T, b *Broker, box *memoryBox) {
	t.Helper()
	m := b.Manifest
	at := b.Clock.Now()
	put(t, box, m.Job()+"/setup-ready.json", Setup{RunID: m.RunID, PublicSHA: m.PublicSHA, Job: "caller-1", CallerID: m.CallerID, Attempt: m.Attempt})
	sessions := []string{"one"}
	if m.Phase == AdmissionBoundary {
		sessions = []string{"two", "three"}
	}
	put(t, box, m.Job()+"/ready.json", Ready{SchemaVersion: 1, RunID: m.RunID, PublicSHA: m.PublicSHA, Job: "caller-1", CallerID: m.CallerID, Attempt: m.Attempt, Sessions: sessions, PID: 1, Host: fixtureHost(at)})
	probes, _ := json.Marshal(fixtureBurst(at))
	for n, op := range []string{"caller-probes", "caller-start"} {
		put(t, box, ackKey(m.Job(), n), Ack{RunID: m.RunID, Job: "caller-1", Sequence: n, OperationID: op, OK: true, ClockProbes: probes})
	}
}
func TestLoadBoundaryBeforeBeginAndAfterInactive(t *testing.T) {
	ctx := context.Background()
	m := fixtureManifest()
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	f := &testFence{}
	box := &memoryBox{}
	survivor, _ := NewBroker(m, NewMailbox(box), clock, f)
	m.Phase = AdmissionBoundary
	m.Plan = NewPlan(AdmissionBoundary, 60, m.PublicSHA)
	m.CallerID = "2"
	boundary, _ := NewBroker(m, NewMailbox(box), clock, f)
	seedBroker(t, survivor, box)
	seedBroker(t, boundary, box)
	health, ready, boundaryReady, inactiveRead := false, false, false, false
	var launches []Phase
	l := launcherFuncs{ready: func() operator.Certainty {
		if !health {
			t.Fatal("MUTATION: health-before-launch")
		}
		ready = true
		return operator.Succeeded
	}, start: func(i Identity) StartResult {
		if !ready {
			t.Fatal("before readiness")
		}
		launches = append(launches, i.Phase)

		return StartResult{Started, operator.Succeeded, i.Unit()}
	}, stop: func(i Identity) StopResult {
		inactiveRead = true
		return StopResult{true, operator.Succeeded, i.Unit()}
	}}
	box.onRead = func(key string) {
		if key == boundary.Manifest.Job()+"/setup-ready.json" {
			boundaryReady = true
		}
	}
	box.onCreate = func(key string, _ []byte) {
		if strings.HasSuffix(key, "/feed/begin.json") && !boundaryReady {
			t.Fatal("MUTATION: boundary-before-begin")
		}
		if key == sequenceKey(boundary.Manifest.Root(), -1) && !inactiveRead {
			t.Fatal("MUTATION: boundary-after-inactive")
		}
	}
	if err := PrepareCallers(ctx, l, survivor, boundary, strings.Repeat("x", 64), clock.at.Add(time.Minute), func(context.Context) error { health = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(launches) != 2 || launches[0] != Survivor || launches[1] != AdmissionBoundary {
		t.Fatal("MUTATION: boundary-before-begin", launches)
	}
	if err := BeginBoundary(ctx, l, survivor, boundary, strings.Repeat("x", 64), clock.at.Add(time.Minute)); err == nil {
		t.Fatal("boundary before terminal survivor")
	}
	put(t, box, survivor.Manifest.Job()+"/finished.json", map[string]any{"runId": survivor.Manifest.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": survivor.Registration.Sessions})
	put(t, box, survivor.Manifest.Job()+"/caller.jsonl", "unused")
	box.data[survivor.Manifest.Job()+"/caller.jsonl"] = []byte("raw report")
	if _, err := survivor.Collect(ctx, clock.at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := BeginBoundary(ctx, l, survivor, boundary, strings.Repeat("x", 64), clock.at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Even an inactive read cannot authorize the next phase after a lost stop.
	f = &testFence{}
	survivor.Fence = f
	boundary.Fence = f
	l.stop = func(i Identity) StopResult { return StopResult{true, operator.Unknown, i.Unit()} }
	before := len(box.creates)
	if err := BeginBoundary(ctx, l, survivor, boundary, strings.Repeat("x", 64), clock.at.Add(time.Minute)); err == nil || len(box.creates) != before || !f.Latched() {
		t.Fatal("MUTATION: boundary-unknown-stop")
	}
}

func TestPreallocatedLaunchAndSingleBoundary(t *testing.T) {
	i := fixtureManifest().Identity
	i.Phase = AdmissionBoundary
	l := &FixedLauncher{configs: map[Identity]LaunchConfig{i: fixtureLaunchConfig()}, Mailbox: NewMailbox(&memoryBox{}), Fence: &testFence{}, ready: true}
	submissions := 0
	l.Exec = execFunc(func(_ context.Context, _ []byte, _ time.Time) Execution {
		key := i.RunID + "/" + string(i.Gate) + "/" + string(i.Phase)
		reserved, ok := l.starts[key]
		if !ok || reserved.Unit != i.Unit() || reserved.State != StartUnknown {
			t.Fatal("MUTATION: identity-before-submission")
		}
		submissions++
		return Execution{Certainty: operator.Succeeded, Output: "caller-started:" + i.Unit() + "\n"}
	})
	end := time.Now().Add(time.Minute)
	if l.Start(context.Background(), i, i.Phase, end).State != Started {
		t.Fatal("boundary start")
	}
	i.CallerID = "2"
	l.configs[i] = fixtureLaunchConfig()
	_ = l.Start(context.Background(), i, i.Phase, end)
	if submissions != 1 {
		t.Fatal("MUTATION: single-boundary-launch")
	}
}
func TestFarPrecheckNeverDials(t *testing.T) {
	m := fixtureManifest()
	box := &memoryBox{}
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	b, _ := NewBroker(m, NewMailbox(box), clock, &testFence{})
	seedBroker(t, b, box)
	put(t, box, m.Job()+"/setup-ready.json", Setup{RunID: m.RunID, PublicSHA: m.PublicSHA, Job: "caller-1", CallerID: m.CallerID, Attempt: m.Attempt, SignalRTTsNS: []int64{10000001}})
	l := launcherFuncs{ready: func() operator.Certainty { return operator.Succeeded }, start: func(i Identity) StartResult { return StartResult{Started, operator.Succeeded, i.Unit()} }}
	box.onCreate = func(key string, _ []byte) {
		if strings.HasSuffix(key, "/feed/begin.json") {
			t.Fatal("MUTATION: far-precheck-dial")
		}
	}
	if err := PrepareCallers(context.Background(), l, b, nil, strings.Repeat("x", 64), clock.at.Add(time.Minute), func(context.Context) error { return nil }); err == nil {
		t.Fatal("far caller accepted")
	}
}

func TestStopOnlyTouchesReservedIdentity(t *testing.T) {
	box := &memoryBox{}
	i := fixtureManifest().Identity
	l := &FixedLauncher{configs: map[Identity]LaunchConfig{i: fixtureLaunchConfig()}, Mailbox: NewMailbox(box), Fence: &testFence{}, ready: true}
	l.Exec = execFunc(func(context.Context, []byte, time.Time) Execution {
		t.Fatal("MUTATION: unowned-caller-stop")
		return Execution{Certainty: operator.Unknown}
	})
	result := l.Stop(context.Background(), i, time.Now().Add(time.Second))
	if result.Certainty != operator.Failed || len(box.creates) != 0 {
		t.Fatal("MUTATION: unowned-caller-stop")
	}
}

func TestSharedHangupReservationAcrossAPIs(t *testing.T) {
	for _, first := range []string{"broker", "launcher"} {
		t.Run(first, func(t *testing.T) {
			ctx := context.Background()
			m := fixtureManifest()
			box := &memoryBox{result: CreateUnknown}
			mailbox := NewMailbox(box)
			fence := &testFence{}
			broker, err := NewBroker(m, mailbox, &testClock{time.UnixMilli(m.CreatedAt)}, fence)
			if err != nil {
				t.Fatal(err)
			}
			executor := execFunc(func(_ context.Context, script []byte, _ time.Time) Execution {
				if strings.Contains(string(script), "systemctl stop") {
					t.Fatal("stop after lost hangup")
				}
				if strings.Contains(string(script), "systemd-run") {
					return Execution{Certainty: operator.Succeeded, Output: "caller-started:" + m.Unit() + "\n"}
				}
				return Execution{Certainty: operator.Succeeded, Output: "LoadState=loaded\nActiveState=inactive\n"}
			})
			launcher, err := NewFixedLauncher(map[Identity]LaunchConfig{m.Identity: fixtureLaunchConfig()}, executor, mailbox, fence)
			if err != nil {
				t.Fatal(err)
			}
			launcher.ready = true
			end := time.Now().Add(time.Minute)
			if launcher.Start(ctx, m.Identity, m.Phase, end).State != Started {
				t.Fatal("start")
			}
			box.onCreate = func(key string, _ []byte) {
				if state, exists := mailbox.hangups[key]; !exists || state != CreateUnknown {
					t.Fatal("MUTATION: hangup-reserved-before-send")
				}
			}
			hangup := func() {
				if broker.Hangup(ctx, "caller-stop") {
					t.Fatal("lost broker hangup confirmed")
				}
			}
			stop := func() {
				result := launcher.Stop(ctx, m.Identity, end)
				if result.Certainty != operator.Unknown || !result.Inactive {
					t.Fatal(result)
				}
			}
			if first == "broker" {
				hangup()
				stop()
			} else {
				stop()
				hangup()
			}
			hangup()
			stop()
			if len(box.creates) != 1 || !fence.Latched() {
				t.Fatal("MUTATION: shared-hangup-once", box.creates)
			}
		})
	}
}

func TestBoundaryRequiresSuccessfulCollection(t *testing.T) {
	for _, scenario := range []string{"missing-report", "mismatched-session", "hangup-only", "stopped", "nonzero", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			m := fixtureManifest()
			at := time.UnixMilli(m.CreatedAt)
			box := &memoryBox{}
			mailbox := NewMailbox(box)
			fence := &testFence{}
			clock := &testClock{at}
			survivor, _ := NewBroker(m, mailbox, clock, fence)
			registration, err := Register(m.Plan, m.RunID, []string{"one"}, fixtureBurst(at), fixtureHost(at), at, nil)
			if err != nil {
				t.Fatal(err)
			}
			survivor.Registration = &registration
			m.Phase = AdmissionBoundary
			m.Plan = NewPlan(m.Phase, 60, m.PublicSHA)
			m.CallerID = "2"
			boundary, _ := NewBroker(m, mailbox, clock, fence)
			seedBroker(t, boundary, box)
			box.onCreate = func(key string, _ []byte) {
				if key == sequenceKey(boundary.Manifest.Root(), -1) {
					t.Fatal("MUTATION: boundary-needs-collection")
				}
			}
			if scenario == "hangup-only" {
				survivor.Hangup(ctx, "caller-stop")
			} else {
				sessions := []string{"one"}
				if scenario == "mismatched-session" {
					sessions = []string{"other"}
				}
				completion := map[string]any{"runId": m.RunID, "job": "caller-1", "done": scenario != "incomplete", "exitCode": 0, "sessions": sessions, "stopped": scenario == "stopped"}
				if scenario == "nonzero" {
					completion["exitCode"] = 1
				}
				put(t, box, survivor.Manifest.Job()+"/finished.json", completion)
				if scenario != "missing-report" {
					box.data[survivor.Manifest.Job()+"/caller.jsonl"] = []byte("raw report")
				}
				if raw, err := survivor.Collect(ctx, at.Add(time.Minute)); err == nil || (scenario != "missing-report" && raw != "raw report") {
					t.Fatal("MUTATION: collection-requires-final-evidence")
				}
			}
			stops := 0
			launcher := launcherFuncs{stop: func(i Identity) StopResult { stops++; return StopResult{true, operator.Succeeded, i.Unit()} }}
			if err := BeginBoundary(ctx, launcher, survivor, boundary, strings.Repeat("x", 64), at.Add(time.Minute)); err == nil || stops != 0 {
				t.Fatal("MUTATION: boundary-needs-collection", err, stops)
			}
			if _, ok := box.data[sequenceKey(boundary.Manifest.Root(), -1)]; ok {
				t.Fatal("boundary dialed")
			}
		})
	}
}

func TestBoundarySetupRequiredBeforeSurvivorBegin(t *testing.T) {
	for _, scenario := range []string{"missing", "mismatched"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			m := fixtureManifest()
			clock := &testClock{time.UnixMilli(m.CreatedAt)}
			box := &memoryBox{}
			mailbox := NewMailbox(box)
			fence := &testFence{}
			survivor, _ := NewBroker(m, mailbox, clock, fence)
			seedBroker(t, survivor, box)
			m.Phase = AdmissionBoundary
			m.Plan = NewPlan(m.Phase, 60, m.PublicSHA)
			m.CallerID = "2"
			boundary, _ := NewBroker(m, mailbox, clock, fence)
			seedBroker(t, boundary, box)
			delete(box.data, boundary.Manifest.Job()+"/setup-ready.json")
			if scenario == "mismatched" {
				put(t, box, boundary.Manifest.Job()+"/setup-ready.json", Setup{RunID: "run-other"})
			}
			box.onCreate = func(key string, _ []byte) {
				if key == sequenceKey(survivor.Manifest.Root(), -1) {
					t.Fatal("MUTATION: boundary-setup-before-dial")
				}
			}
			launcher := launcherFuncs{ready: func() operator.Certainty { return operator.Succeeded }, start: func(i Identity) StartResult { return StartResult{Started, operator.Succeeded, i.Unit()} }}
			err := PrepareCallers(ctx, launcher, survivor, boundary, strings.Repeat("x", 64), clock.at.Add(time.Minute), func(context.Context) error { return nil })
			if err == nil || survivor.Registration != nil {
				t.Fatal("MUTATION: boundary-setup-before-dial", err)
			}
		})
	}
}

func TestLauncherBindsBothPhaseConfigurations(t *testing.T) {
	ctx := context.Background()
	m := fixtureManifest()
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	box := &memoryBox{}
	mailbox := NewMailbox(box)
	fence := &testFence{}
	survivor, _ := NewBroker(m, mailbox, clock, fence)
	seedBroker(t, survivor, box)
	m.Phase = AdmissionBoundary
	m.Plan = NewPlan(m.Phase, 60, m.PublicSHA)
	m.CallerID = "2"
	boundary, _ := NewBroker(m, mailbox, clock, fence)
	seedBroker(t, boundary, box)
	configs := map[Identity]LaunchConfig{}
	for _, b := range []*Broker{survivor, boundary} {
		c := fixtureLaunchConfig()
		c.ConfigPath = "/opt/caller/" + string(b.Manifest.Phase) + ".json"
		configs[b.Manifest.Identity] = c
	}
	launches := 0
	executor := execFunc(func(_ context.Context, script []byte, _ time.Time) Execution {
		text := string(script)
		if strings.Contains(text, "caller-ready") {
			return Execution{Certainty: operator.Succeeded, Output: "caller-ready\n"}
		}
		for _, b := range []*Broker{survivor, boundary} {
			if strings.Contains(text, "unit="+quote(b.Manifest.Unit())) {
				encoded, _ := json.Marshal(b.Manifest.Identity)
				if !strings.Contains(text, "cp "+quote("/opt/caller/"+string(b.Manifest.Phase)+".json")) || !strings.Contains(text, "-identity "+quote(string(encoded))) {
					t.Fatal("MUTATION: phase-config-binding", text)
				}
				launches++
				return Execution{Certainty: operator.Succeeded, Output: "caller-started:" + b.Manifest.Unit() + "\n"}
			}
		}
		t.Fatal("unexpected script")
		return Execution{}
	})
	launcher, err := NewFixedLauncher(configs, executor, mailbox, fence)
	if err != nil {
		t.Fatal(err)
	}
	configs[boundary.Manifest.Identity] = fixtureLaunchConfig() // A caller cannot change the immutable copy.
	if err = PrepareCallers(ctx, launcher, survivor, boundary, strings.Repeat("x", 64), time.Now().Add(time.Minute), func(context.Context) error { return nil }); err != nil || launches != 2 {
		t.Fatal("MUTATION: phase-config-binding", err, launches)
	}
}

func TestCallerEvidenceRejectsAmbiguousMembers(t *testing.T) {
	for _, member := range []string{`"exitCode":1,"exitCode":0`, `"exitCode":1,"ExitCode":0`} {
		m := fixtureManifest()
		at := time.UnixMilli(m.CreatedAt)
		box := &memoryBox{}
		broker, _ := NewBroker(m, NewMailbox(box), &testClock{at}, &testFence{})
		registration, err := Register(m.Plan, m.RunID, []string{"one"}, fixtureBurst(at), fixtureHost(at), at, nil)
		if err != nil {
			t.Fatal(err)
		}
		broker.Registration = &registration
		raw := `{"runId":"run-fixture","job":"caller-1","done":true,"stopped":false,` + member + `,"sessions":["one"]}`
		box.data = map[string][]byte{m.Job() + "/finished.json": []byte(raw), m.Job() + "/caller.jsonl": []byte("raw report")}
		if _, err := broker.Collect(context.Background(), at.Add(time.Minute)); err == nil || broker.collected {
			t.Fatal("MUTATION: caller-json-ambiguity", member)
		}
	}
}

func TestFeedHonorsProcedureDeadline(t *testing.T) {
	m := fixtureManifest()
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	box := &memoryBox{}
	broker, err := NewBroker(m, NewMailbox(box), clock, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = broker.Feed(context.Background(), "operation-end", Payload{Kind: "finish"}, clock.Now()); err == nil || len(box.creates) != 0 || broker.sequence != 0 {
		t.Fatal("MUTATION: caller-procedure-deadline", err, len(box.creates))
	}
}

func TestCancelledCollectionRetainsPublishedDirectoryReport(t *testing.T) {
	directory, err := OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	m := fixtureManifest()
	b, err := NewBroker(m, NewMailbox(directory), &testClock{time.UnixMilli(m.CreatedAt)}, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	b.Registration = &Registration{Sessions: []string{"one"}}
	raw := []byte(`{"DecryptionFailures":{"AuthTag":1,"Replay":0,"Other":0}}`)
	key := m.Job() + "/caller.jsonl"
	if directory.Create(context.Background(), key, raw) != Committed {
		t.Fatal("publish report")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := directory.Read(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatal("directory must honor cancellation", err)
	}
	got, err := b.Collect(ctx, time.UnixMilli(m.CreatedAt).Add(time.Minute))
	if !errors.Is(err, context.Canceled) || got != string(raw) || b.collected {
		t.Fatal("MUTATION: cancelled-directory-retention", got, err, b.collected)
	}
}

func TestRetentionCannotExtendCollectionDeadline(t *testing.T) {
	m := fixtureManifest()
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	end := clock.at.Add(time.Minute)
	box := &memoryBox{}
	put(t, box, m.Job()+"/finished.json", map[string]any{"runId": m.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": []string{"one"}})
	box.data[m.Job()+"/caller.jsonl"] = []byte("published failure evidence")
	box.onRead = func(key string) {
		if strings.HasSuffix(key, "/caller.jsonl") {
			clock.at = end
		}
	}
	b, err := NewBroker(m, NewMailbox(box), clock, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	b.Registration = &Registration{Sessions: []string{"one"}}
	raw, err := b.Collect(context.Background(), end)
	if raw != "published failure evidence" || err == nil || b.collected {
		t.Fatal("MUTATION: retention-collection-deadline", raw, err, b.collected)
	}
}
