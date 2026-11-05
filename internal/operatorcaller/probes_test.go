package operatorcaller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

// probeAt uses the longest wall encoding: nine fraction digits and an offset.
func probeAt(i int) callharness.ClockProbe {
	send := time.Date(2026, 11, 5, 1, 0, 0, 123456789, time.FixedZone("PST", -8*3600)).Add(time.Duration(i) * 250 * time.Millisecond)
	return callharness.ClockProbe{CallerSend: send, CallerReceive: send.Add(1234567 * time.Nanosecond), ServerReceive: send.Add(345678 * time.Nanosecond), ServerSend: send.Add(456789 * time.Nanosecond)}
}
func probeDelta(t *testing.T, from int, probes ...callharness.ClockProbe) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ProbeDelta{From: from, Probes: append([]callharness.ClockProbe{}, probes...)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAppendProbesContinuity(t *testing.T) {
	p := make([]callharness.ClockProbe, 6)
	for i := range p {
		p[i] = probeAt(i)
	}
	confirmed := append([]callharness.ClockProbe{}, p[:3]...)
	changed := p[2]
	changed.ServerSend = changed.ServerSend.Add(time.Nanosecond)
	for _, c := range []struct {
		name, err string
		raw       json.RawMessage
	}{
		{"gap", "clock-probe-gap", probeDelta(t, 4, p[4], p[5])},
		{"negative cursor", "clock-probe-gap", probeDelta(t, -1, p[3])},
		{"changed repeat", "clock-probe-repeat", probeDelta(t, 2, changed, p[3])},
		{"reorder", "clock-probe-order", probeDelta(t, 3, p[4], p[3])},
		{"repeat inside delta", "clock-probe-order", probeDelta(t, 3, p[3], p[3])},
		{"stale probe", "clock-probe-order", probeDelta(t, 3, p[1])},
		{"missing probes", "clock-probe-gap", json.RawMessage(`{"from":3}`)},
		{"null probes", "clock-probe-gap", json.RawMessage(`{"from":3,"probes":null}`)},
		{"missing from", "clock-probe-gap", json.RawMessage(`{"probes":[]}`)},
		{"missing member", "clock-probe-gap", json.RawMessage(`{"from":3,"probes":[{"caller_send":"2026-11-05T01:00:01Z","caller_receive":"2026-11-05T01:00:01Z","server_receive":"2026-11-05T01:00:01Z"}]}`)},
		{"legacy burst", "clock-probe-gap", json.RawMessage(`{"startedAt":"2026-11-05T01:00:00Z","finishedAt":"2026-11-05T01:00:00Z","probes":[]}`)},
	} {
		got, err := AppendProbes(confirmed, c.raw)
		if err == nil || err.Error() != c.err || len(got) != 3 {
			t.Fatalf("MUTATION: probe-continuity %s: %v %d", c.name, err, len(got))
		}
	}
	// A retried ack repeats confirmed probes exactly; they are dropped.
	got, err := AppendProbes(confirmed, probeDelta(t, 1, p[1:]...))
	if err != nil || len(got) != 6 || len(confirmed) != 3 {
		t.Fatalf("MUTATION: probe-exact-repeat-dropped: %v %d", err, len(got))
	}
	for i := range got {
		if !bytes.Equal(marshal(t, got[i]), marshal(t, p[i])) {
			t.Fatal("MUTATION: probe-exact-repeat-dropped", i)
		}
	}
}
func marshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// probeRig runs the real broker, bridge delivery, child handler and frame-limited
// loopback GET. Acks travel as protocol mailbox frames without a spool.
type probeRig struct {
	t        *testing.T
	broker   *Broker
	box      *memoryBox
	mu       sync.Mutex
	child    []callharness.ClockProbe
	server   *httptest.Server
	cursors  []int
	maxGet   int
	maxFrame int
	lose     bool
	lost     []Ack
}

func newProbeRig(t *testing.T) *probeRig {
	r := &probeRig{t: t, box: &memoryBox{data: map[string][]byte{}}}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		probes := append([]callharness.ClockProbe{}, r.child...)
		r.mu.Unlock()
		recorder := httptest.NewRecorder()
		loadgen.ServeClockProbes(recorder, req, probes)
		if req.URL.Query().Has("after") {
			r.mu.Lock()
			r.maxGet = max(r.maxGet, recorder.Body.Len())
			r.mu.Unlock()
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(r.server.Close)
	m := fixtureManifest()
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	b, err := NewBroker(m, NewMailbox(r.box), clock, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	b.Registration = &Registration{Near: true, Sessions: []string{"one"}}
	r.broker = b
	bridge := &ProtocolMailbox{Peer: &mailboxPeer{reply: r.store}, Session: "caller", End: time.Now().Add(time.Hour)}
	r.box.onCreate = func(key string, data []byte) {
		var feed Feed
		if !bytes.Contains([]byte(key), []byte("/feed/")) || json.Unmarshal(data, &feed) != nil || feed.Sequence < 0 {
			return
		}
		r.cursors = append(r.cursors, feed.Payload.ClockProbesAfter)
		ack, err := Deliver(context.Background(), feed, []string{"one"}, r.local, func() { t.Fatal("unexpected hangup") }, clock)
		if err != nil {
			t.Fatal(err)
		}
		if r.lose {
			// The ack never reaches the operator; its cursor stays put.
			r.lost = append(r.lost, ack)
			ack = Ack{RunID: ack.RunID, Job: ack.Job, Sequence: ack.Sequence, OperationID: ack.OperationID, Error: "lost"}
		}
		if createJSON(context.Background(), bridge, ackKey(m.Job(), feed.Sequence), ack) != Committed {
			t.Fatal("MUTATION: probe-frame-size: ack needs a spool or the frame limit")
		}
	}
	return r
}
func (r *probeRig) local(ctx context.Context, path string, in, out any) error {
	if in != nil {
		return nil
	}
	return localWithBase(ctx, r.server.URL, r.server.Client(), path, out)
}

// store is the caller-side mailbox adapter. It measures each wire frame.
func (r *probeRig) store(f operator.Frame) (operator.Frame, error) {
	line, err := json.Marshal(f)
	if err != nil {
		return operator.Frame{}, err
	}
	r.maxFrame = max(r.maxFrame, len(line)+1)
	if len(line)+1 > operator.MaxFrameBytes {
		r.t.Fatalf("MUTATION: probe-frame-size %d", len(line)+1)
	}
	var args MailboxArgs
	if err = json.Unmarshal(f.Request.Args, &args); err != nil || args.Blob != nil {
		r.t.Fatal("ack needed a spool", err)
	}
	r.box.data[args.Key] = args.Data
	zero := 0
	return operator.Frame{Type: "result", Session: f.Session, Result: &operator.Reply{ID: f.Request.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: json.RawMessage(`{"state":"committed"}`)}}, nil
}
func (r *probeRig) grow(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for range n {
		r.child = append(r.child, probeAt(len(r.child)))
	}
}
func (r *probeRig) arm(n int) error {
	op := fmt.Sprintf("drain-w1-%05d", n)
	return r.broker.Arm(context.Background(), op, []callharness.OperationIdentity{{RunID: r.broker.Manifest.RunID, OperationID: op, SessionID: "one", Worker: "w1", Epoch: 1}}, time.UnixMilli(r.broker.Manifest.Deadline))
}

// fullList is the evidence the full-list contract carried in its last arm ack.
func (r *probeRig) fullList() []byte {
	response, err := r.server.Client().Get(r.server.URL + "/clock-probes")
	if err != nil {
		r.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	ack := marshal(r.t, Ack{ClockProbes: raw})
	var old struct {
		ClockProbes json.RawMessage `json:"clockProbes"`
	}
	if err = json.Unmarshal(ack, &old); err != nil {
		r.t.Fatal(err)
	}
	return old.ClockProbes
}

func TestIncrementalProbesMatchFullList(t *testing.T) {
	r := newProbeRig(t)
	for n, grow := range []int{7, 0, 40, 1, 63} {
		r.grow(grow)
		if err := r.arm(n); err != nil {
			t.Fatal("MUTATION: probe-confirmed-cursor", err)
		}
	}
	want := []int{0, 7, 7, 47, 48}
	for i, c := range want {
		if r.cursors[i] != c {
			t.Fatalf("MUTATION: probe-confirmed-cursor %v", r.cursors)
		}
	}
	if got := marshal(t, r.broker.ClockProbes); !bytes.Equal(got, r.fullList()) {
		t.Fatalf("MUTATION: probe-full-list-identity\ngot  %.300s\nwant %.300s", got, r.fullList())
	}
}

// Confirmed means the operator appended the ack's delta. A lost ack confirms
// nothing, so a retry re-sends from the same cursor and nothing is dropped.
func TestLostProbeAckResendsFromConfirmedCursor(t *testing.T) {
	r := newProbeRig(t)
	r.grow(5)
	if err := r.arm(0); err != nil {
		t.Fatal(err)
	}
	r.grow(4)
	r.lose = true
	if err := r.arm(1); err == nil {
		t.Fatal("lost ack accepted")
	}
	if len(r.broker.ClockProbes) != 5 || len(r.lost) != 1 {
		t.Fatalf("MUTATION: probe-lost-ack-confirmed %d", len(r.broker.ClockProbes))
	}
	// The retried delivery of the same feed starts at the confirmed cursor and
	// now also carries a later probe.
	r.lose = false
	r.grow(1)
	var feed Feed
	if err := json.Unmarshal(r.box.data[sequenceKey(r.broker.Manifest.Root(), 1)], &feed); err != nil {
		t.Fatal(err)
	}
	retry, err := Deliver(context.Background(), feed, []string{"one"}, r.local, func() {}, r.broker.Clock)
	if err != nil || feed.Payload.ClockProbesAfter != 5 {
		t.Fatal("MUTATION: probe-retry-cursor", err, feed.Payload.ClockProbesAfter)
	}
	// Either order of a late lost ack and its retry yields the full list once.
	for _, order := range [][]Ack{{retry}, {r.lost[0], retry}, {retry, r.lost[0]}} {
		probes := append([]callharness.ClockProbe{}, r.broker.ClockProbes...)
		for _, ack := range order {
			if probes, err = AppendProbes(probes, ack.ClockProbes); err != nil {
				t.Fatal("MUTATION: probe-retry-duplicates", err)
			}
		}
		if !bytes.Equal(marshal(t, probes), r.fullList()) {
			t.Fatal("MUTATION: probe-retry-dropped", len(probes))
		}
	}
}

func TestProbeContinuityErrorLatches(t *testing.T) {
	r := newProbeRig(t)
	r.grow(3)
	if err := r.arm(0); err != nil {
		t.Fatal(err)
	}
	// The child serves its next two probes in the wrong order.
	r.grow(2)
	r.mu.Lock()
	r.child[3], r.child[4] = r.child[4], r.child[3]
	r.mu.Unlock()
	if err := r.arm(1); err == nil || err.Error() != "clock-probe-order" {
		t.Fatal("MUTATION: probe-continuity-arm", err)
	}
	if len(r.broker.ClockProbes) != 3 {
		t.Fatal("MUTATION: probe-continuity-partial", len(r.broker.ClockProbes))
	}
	feeds := len(r.cursors)
	if err := r.arm(2); err == nil || len(r.cursors) != feeds {
		t.Fatal("MUTATION: probe-continuity-latch", err)
	}
}

// Worst case per arm: all probes since the previous ack are undelivered. That
// is one event's 63 burst probes plus 22 minutes of 10 s background probes.
func TestProbeFramesUnderLimitAt16384(t *testing.T) {
	r := newProbeRig(t)
	// CollectProbes bounds real loopback requests by its deadline.
	clock := &testClock{time.Now()}
	start := clock.Now()
	// Registration: the probe feed polls incrementally while probes arrive.
	r.grow(1)
	burst, err := CollectProbes(context.Background(), ClockBurstCount, start.Add(2*time.Minute), func(ctx context.Context, path string, in, out any) error {
		r.mu.Lock()
		due := len(r.child) < int(clock.Now().Sub(start)/time.Second)+1
		r.mu.Unlock()
		if due {
			r.grow(1)
		}
		return r.local(ctx, path, in, out)
	}, clock)
	if err != nil || len(burst.Probes) != ClockBurstCount {
		t.Fatal(err)
	}
	r.mu.Lock()
	feedGet := r.maxGet
	r.mu.Unlock()
	if feedGet > 512 {
		t.Fatal("MUTATION: probe-feed-incremental", feedGet)
	}
	const perArm = 63 + int(CallerMaximum/(10*time.Second))
	for n := 0; len(r.broker.ClockProbes) < 16384; n++ {
		r.grow(min(perArm, 16384-len(r.broker.ClockProbes)))
		if err := r.arm(n); err != nil {
			t.Fatal("MUTATION: probe-frame-size", n, err)
		}
	}
	if !bytes.Equal(marshal(t, r.broker.ClockProbes), r.fullList()) {
		t.Fatal("MUTATION: probe-full-list-identity")
	}
	t.Logf("16384 probes over %d arms: largest cursor response %d bytes, largest ack frame %d bytes (limit %d)", len(r.cursors), r.maxGet, r.maxFrame, operator.MaxFrameBytes)
}

func TestManifestVersionRefusesOtherContracts(t *testing.T) {
	m := fixtureManifest()
	now := time.UnixMilli(m.CreatedAt)
	if err := m.Validate(m.Identity, now); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 3} {
		m.Version = version
		if err := m.Validate(m.Identity, now); err == nil {
			t.Fatal("MUTATION: caller-contract-version", version)
		}
	}
}
