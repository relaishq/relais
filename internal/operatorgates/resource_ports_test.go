package operatorgates

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

func TestResourcePortUnknownIsNeverFailure(t *testing.T) {
	s, _ := fixtureSession(t)
	s.capabilities[operator.CallerReady] = true
	peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
		return operator.Reply{Transport: operator.Unknown, ExitCode: new(int), Payload: json.RawMessage(`{"ready":true}`)}
	}}
	p := ResourcePorts{Session: s, Peer: peer, End: time.Minute}
	if got := p.Ready(context.Background(), s.clock.Wall().Add(time.Minute)); got != operator.Unknown || !s.Latched() || peer.sent != 1 {
		t.Fatal("MUTATION: resource-unknown-is-unknown", got, s.Latched(), peer.sent)
	}
}
func TestResourceMailboxPayloadsAndLargeEvidence(t *testing.T) {
	for _, mode := range []string{"absent", "large", "unknown-absence", "conflict", "unknown-create", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixtureSession(t)
			data := []byte(strings.Repeat("e", 300000))
			peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
				var args operatorcaller.MailboxArgs
				if json.Unmarshal(q.Args, &args) != nil || args.Key != "caller/evidence" {
					t.Fatal(q)
				}
				found := mode != "absent" && mode != "unknown-absence"
				value := operatorcaller.MailboxValue{Found: &found}
				if mode == "large" {
					value.Data = data
				}
				if strings.Contains(mode, "create") || mode == "conflict" {
					value = operatorcaller.MailboxValue{State: operatorcaller.Conflict}
				}
				payload, _ := json.Marshal(value)
				if mode == "malformed" {
					payload = json.RawMessage(`{}`)
				}
				c := operator.Succeeded
				if strings.HasPrefix(mode, "unknown") {
					c = operator.Unknown
				}
				return operator.Reply{Transport: c, ExitCode: new(int), Payload: payload}
			}}
			p := ResourcePorts{Session: s, Peer: peer, End: time.Minute}
			if mode == "conflict" || mode == "unknown-create" {
				got := p.Create(context.Background(), "caller/evidence", []byte("proof"))
				want := operatorcaller.Conflict
				if mode == "unknown-create" {
					want = operatorcaller.CreateUnknown
				}
				if got != want {
					t.Fatal("create certainty", got, want)
				}
			} else {
				got, err := p.Read(context.Background(), "caller/evidence")
				if mode == "large" && (err != nil || string(got.Data) != string(data)) {
					t.Fatal("large evidence lost", err)
				}
				if mode == "absent" && (err != nil || got.Found || len(got.Data) != 0) {
					t.Fatal("absence proof lost", got, err)
				}
				if (mode == "unknown-absence" || mode == "malformed") && (err == nil || !s.Latched()) {
					t.Fatal("unknown became absence", got, err)
				}
			}
		})
	}
}
func TestResourceStartStopReservations(t *testing.T) {
	s, _ := fixtureSession(t)
	id := operatorcaller.Identity{RunID: "run-port", Gate: operator.Rollout, Phase: operatorcaller.Survivor, CallerID: "1", Attempt: "1", PublicSHA: strings.Repeat("a", 40)}
	peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: json.RawMessage(`{"start":"started","inactive":true}`)}
	}}
	p := ResourcePorts{Session: s, Peer: peer, End: time.Minute, Manifest: operatorcaller.Manifest{Identity: id}}
	for range 2 {
		if got := p.Start(context.Background(), id, id.Phase, s.clock.Wall().Add(time.Minute)); got.State != operatorcaller.Started {
			t.Fatal(got)
		}
		if got := p.Stop(context.Background(), id, s.clock.Wall().Add(time.Minute)); !got.Inactive || got.Certainty != operator.Succeeded {
			t.Fatal(got)
		}
	}
	if peer.sent != 2 {
		t.Fatal("one reservation per action", peer.sent)
	}
}
func TestResourceMailboxConcurrentExchanges(t *testing.T) {
	// The actual caller collection goroutine shares a session with host steps.
	// Exercise that serialization and concurrent fatal-fence notifications under race.
	// Use a real clock so the test itself never races a mutable fake clock.
	base, _ := fixtureSession(t)
	base.clock = NewRealClock()
	base.Budget.clock = base.clock
	peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: json.RawMessage(`{"found":false}`)}
	}}
	p := ResourcePorts{Session: base, Peer: peer, End: time.Minute}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 20 {
				_, _ = p.Read(context.Background(), "caller/evidence")
				_ = base.Latched()
			}
		})
	}
	wg.Wait()
	if peer.sent != 40 || base.Latched() {
		t.Fatal("concurrent mailbox exchange", peer.sent)
	}
}

func TestResourceSequenceStreamsBoundedFinal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	er, sw := io.Pipe()
	sr, ew := io.Pipe()
	engine := operatortransport.NewPipe(er, ew)
	server := operatortransport.NewPipe(sr, sw)
	defer engine.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- ServeSequence(ctx, engine, operator.Hello{Protocol: 1}, func(_ context.Context, s *Session, _ Peer) []operator.GateResult {
			for range 4000 {
				s.record("request", "1", operator.Succeeded, "")
			}
			return []operator.GateResult{{Gate: operator.Rollout, Status: operator.Passed}}
		})
	}()
	if f, err := server.Receive(ctx); err != nil || f.Type != "hello" {
		t.Fatal(f, err)
	}
	now := time.Now()
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Minute).UnixMilli(), 10)}
	if err := server.Send(ctx, operator.Frame{Type: "init", Session: "session", Init: &init}); err != nil {
		t.Fatal(err)
	}
	traces := 0
	for {
		f, err := server.Receive(ctx)
		if err != nil {
			t.Fatal("long sequence could not finish", err)
		}
		if f.Type == "trace" {
			traces++
			continue
		}
		if f.Type != "final" || f.Final.Status != operator.Passed || len(f.Final.Trace) != 0 || traces != 4000 {
			t.Fatal("long sequence final", f, traces)
		}
		break
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestResourceStoreFailoverRequests(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		s, _ := fixtureSession(t)
		s.capabilities[operator.StoreObserve] = true
		s.capabilities[operator.StoreFailover] = true
		operations := []operator.Operation{}
		peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
			operations = append(operations, q.Operation)
			r := operator.Reply{Transport: operator.Succeeded, ExitCode: new(int)}
			if q.Operation == operator.StoreFailover {
				r.Payload = json.RawMessage(`{"completed":true}`)
				if unknown {
					r.Transport = operator.Unknown
				}
			} else {
				primary := "a"
				if len(operations) > 1 {
					primary = "b"
				}
				r.Payload, _ = json.Marshal(StoreObservation{Primary: primary, Available: true, Durability: "sync"})
			}
			return r
		}}
		store := s.OptionalStore(peer)
		before, err := store.Observe(context.Background(), time.Minute)
		if err != nil || before.Primary != "a" {
			t.Fatal("store initial proof", before, err)
		}
		err = store.Failover(context.Background(), time.Minute)
		after, readErr := store.Observe(context.Background(), time.Minute)
		if unknown {
			if code(err) != "command-result-unknown" || readErr == nil || !s.Latched() || len(operations) != 2 {
				t.Fatal("MUTATION: resource-store-unknown", err, readErr, operations)
			}
		} else if err != nil || readErr != nil || after.Primary != "b" || s.Latched() || len(operations) != 3 {
			t.Fatal("store failover proof", after, err, readErr, operations)
		}
	}
}
