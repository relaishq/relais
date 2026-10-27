package operatorgates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

func TestSessionPipeProtocol(t *testing.T) {
	a, b, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c, d, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	engine := operatortransport.NewPipe(a, d)
	adapter := operatortransport.NewPipe(c, b)
	defer engine.Close()
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, engine, operator.Hello{Protocol: 1}, func(ctx context.Context, s *Session, p Peer, g operator.Gate) operator.GateResult {
			end, _ := s.Budget.GateEnd()
			out, err := s.Exchange(ctx, p, operator.HostExecute, "shared", "move", nil, nil, end, func(operator.Reply) operator.Certainty { return operator.Succeeded })
			status := operator.Passed
			if out != operator.Succeeded || err != nil {
				status = operator.Inconclusive
			}
			return operator.GateResult{Gate: g, Status: status}
		})
	}()
	hello, err := adapter.Receive(ctx)
	if err != nil || hello.Type != "hello" {
		t.Fatal(hello, err)
	}
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: "", Capabilities: []operator.Operation{operator.HostExecute}}
	now := time.Now().UnixMilli()
	init.ArmedAtUnixMS = decimal(now)
	init.EndUnixMS = decimal(now + SessionMaximum.Milliseconds())
	if err := adapter.Send(ctx, operator.Frame{Type: "init", Session: "s", Init: &init}); err != nil {
		t.Fatal(err)
	}
	req, err := adapter.Receive(ctx)
	if err != nil || req.Type != "request" {
		t.Fatal(req, err)
	}
	r := reply(*req.Request, operator.Unknown)
	if err := adapter.Send(ctx, operator.Frame{Type: "result", Session: "s", Result: &r}); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := adapter.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == "final" {
			if f.Final.Status != operator.Inconclusive || !f.Final.Latched || f.Final.Complete {
				t.Fatal("pipe unknown passed")
			}
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func decimal(n int64) string { return strconv.FormatInt(n, 10) }

func TestInterruptBeforeRequest(t *testing.T) {
	a, b, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, d, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := operatortransport.NewPipe(a, d)
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, _ := fixtureSession(t)
	_, _ = s.Exchange(ctx, p, operator.HostExecute, "shared", "move", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Succeeded })
	if len(s.requests) != 0 {
		t.Fatal("MUTATION: interrupted-request")
	}
}

type queuedPeer struct {
	frame operator.Frame
	sent  int
}

func (p *queuedPeer) Send(context.Context, operator.Frame) error      { p.sent++; return nil }
func (p *queuedPeer) Receive(context.Context) (operator.Frame, error) { return p.frame, nil }
func (p *queuedPeer) Poll() (operator.Frame, error, bool)             { return p.frame, nil, true }
func (p *queuedPeer) Close() error                                    { return nil }
func TestQueuedProtocolEventBeforeEffect(t *testing.T) {
	for _, kind := range []string{"cancel", "result", "protocol_error"} {
		s, _ := fixtureSession(t)
		p := &queuedPeer{frame: operator.Frame{Type: kind}}
		_, err := s.Exchange(context.Background(), p, operator.HostExecute, "shared", "restore", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Succeeded })
		if err == nil || !s.Latched() || p.sent != 0 || len(s.requests) != 0 {
			t.Fatal("queued protocol event allowed an effect")
		}
	}
}

type responsePeer struct {
	response operator.Reply
	request  *operator.Request
	deadline time.Time
	sent     int
}

func (p *responsePeer) Send(ctx context.Context, f operator.Frame) error {
	p.sent++
	p.request = f.Request
	p.deadline, _ = ctx.Deadline()
	return nil
}
func (p *responsePeer) Receive(context.Context) (operator.Frame, error) {
	r := p.response
	r.ID = p.request.ID
	return operator.Frame{Type: "result", Session: "session", Result: &r}, nil
}
func (p *responsePeer) Poll() (operator.Frame, error, bool) { return operator.Frame{}, nil, false }
func (p *responsePeer) Close() error                        { return nil }
func TestActiveGateDeadlineBeforeEffect(t *testing.T) {
	s, c := fixtureSession(t)
	end, ok := s.BeginGate()
	if !ok {
		t.Fatal("gate did not begin")
	}
	c.Advance(GateMaximum)
	renewed, _ := s.Budget.GateEnd()
	if renewed <= end {
		t.Fatal("test failed to request a renewed window")
	}
	p := &responsePeer{}
	_, err := s.Exchange(context.Background(), p, operator.HostExecute, "shared", "move", nil, nil, renewed, func(operator.Reply) operator.Certainty { return operator.Succeeded })
	if err == nil || p.sent != 0 {
		t.Fatal("MUTATION: active-gate-cutoff")
	}
	if again, ok := s.BeginGate(); ok || again != end {
		t.Fatal("active gate was renewed")
	}
}
func TestExchangeWaitUsesClampedEnd(t *testing.T) {
	s, c := fixtureSession(t)
	_, _ = s.BeginGate()
	c.Advance(19 * time.Minute)
	p := &responsePeer{response: operator.Reply{Transport: operator.Succeeded, ExitCode: new(int)}}
	out, err := s.Exchange(context.Background(), p, operator.HostExecute, "shared", "move", nil, nil, SessionMaximum, func(operator.Reply) operator.Certainty { return operator.Succeeded })
	if err != nil || out != operator.Succeeded {
		t.Fatal(out, err)
	}
	if p.request.RemainingMS != 60000 || time.Until(p.deadline) > 61*time.Second {
		t.Fatal("MUTATION: clamped-wait")
	}
}
func TestExchangeResolvesBlobsBeforeClassification(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	data := []byte(`{"kind":"ok"}`)
	hash := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(dir, "payload"), data, 0600); err != nil {
		t.Fatal(err)
	}
	valid := operator.Blob{Name: "payload", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	for _, mode := range []string{"oversize", "name", "hash-shape", "hash-mismatch", "size", "both", "missing-spool", "valid"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixtureSession(t)
			s.SetBlobRoot(root)
			ref := valid
			p := &responsePeer{response: operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Blob: &ref}}
			switch mode {
			case "oversize":
				ref.Size = operator.MaxBlobBytes + 1
			case "name":
				ref.Name = "../payload"
			case "hash-shape":
				ref.SHA256 = "customer-production-db"
			case "hash-mismatch":
				ref.SHA256 = strings.Repeat("0", 64)
			case "size":
				ref.Size++
			case "both":
				p.response.Payload = json.RawMessage(`{"kind":"ok"}`)
			case "missing-spool":
				s.SetBlobRoot(nil)
			}
			classified := false
			out, err := s.Exchange(context.Background(), p, operator.HostExecute, "shared", "move", nil, nil, time.Minute, func(r operator.Reply) operator.Certainty {
				classified = true
				if r.Blob != nil || !bytes.Equal(r.Payload, data) {
					t.Error("blob was not resolved")
				}
				return operator.Succeeded
			})
			if mode == "valid" {
				if err != nil || out != operator.Succeeded || !classified {
					t.Fatal(out, err)
				}
				return
			}
			if err == nil || out != operator.Unknown || classified || !s.Latched() {
				t.Fatal("MUTATION: blob-before-classification")
			}
			if _, err := s.Issue(operator.HostExecute, "shared", "restore", nil, nil, time.Minute); err == nil {
				t.Fatal("invalid blob allowed a successor")
			}
		})
	}
}

func (p *queuedPeer) Finish(ctx context.Context, build func(error) operator.Frame) error {
	return p.Send(ctx, build(nil))
}
func (p *responsePeer) Finish(ctx context.Context, build func(error) operator.Frame) error {
	return p.Send(ctx, build(nil))
}

// All fault bytes pass through the real reader while Serve is paused after its
// last valid reply. Neither the latch nor the transport state is set by a test helper.
func TestServePublishedFaultFencesFinal(t *testing.T) {
	for _, fault := range []string{"duplicate", "malformed"} {
		for _, phase := range []string{"classifier", "runner"} {
			t.Run(fault+"/"+phase, func(t *testing.T) {
				inputR, inputW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer inputW.Close()
				outputR, outputW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer outputR.Close()
				engine := operatortransport.NewPipe(inputR, outputW)
				defer engine.Close()
				wire := operatortransport.NewCodec(outputR, inputW)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				paused, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				resume := func() { releaseOnce.Do(func() { close(release) }) }
				defer resume()
				owned := make(chan *Session, 1)
				done := make(chan error, 1)
				go func() {
					done <- Serve(ctx, engine, operator.Hello{Protocol: 1}, func(ctx context.Context, s *Session, p Peer, g operator.Gate) operator.GateResult {
						owned <- s
						end, _ := s.BeginGate()
						out, err := s.Exchange(ctx, p, operator.HostExecute, "shared", "move", nil, nil, end, func(operator.Reply) operator.Certainty {
							if phase == "classifier" {
								close(paused)
								<-release
							}
							return operator.Succeeded
						})
						if phase == "runner" {
							close(paused)
							<-release
						}
						if err != nil || out != operator.Succeeded {
							return operator.GateResult{Gate: g, Status: operator.Inconclusive}
						}
						return operator.GateResult{Gate: g, Status: operator.Passed}
					})
				}()
				hello, err := wire.Read()
				if err != nil || hello.Type != "hello" {
					t.Fatal(hello, err)
				}
				now := time.Now().UnixMilli()
				init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: decimal(now), EndUnixMS: decimal(now + SessionMaximum.Milliseconds()), Capabilities: []operator.Operation{operator.HostExecute}}
				if err := wire.Write(operator.Frame{Type: "init", Session: "session", Init: &init}); err != nil {
					t.Fatal(err)
				}
				req, err := wire.Read()
				if err != nil || req.Type != "request" {
					t.Fatal(req, err)
				}
				completed := reply(*req.Request, operator.Succeeded)
				if err := wire.Write(operator.Frame{Type: "result", Session: "session", Result: &completed}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-paused:
				case <-ctx.Done():
					t.Fatal("Serve never reached the pause")
				}
				if fault == "duplicate" {
					err = wire.Write(operator.Frame{Type: "result", Session: "session", Result: &completed})
				} else {
					_, err = io.WriteString(inputW, "{malformed}\n")
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-engine.Faulted():
				case <-ctx.Done():
					t.Fatal("fault not published")
				}
				resume()
				for {
					f, err := wire.Read()
					if err != nil {
						t.Fatal("MUTATION: final-fault-latch", err)
					}
					if f.Type != "final" {
						continue
					}
					if !f.Final.Latched || f.Final.Complete || f.Final.Status != operator.Inconclusive {
						t.Fatal("MUTATION: final-fault-latch")
					}
					break
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if s := <-owned; !s.Latched() {
					t.Fatal("fault changed only the DTO, not the session")
				}
			})
		}
	}
}
func TestServeRunnerContextUsesActiveCutoff(t *testing.T) {
	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputW.Close()
	outputR, outputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outputR.Close()
	engine := operatortransport.NewPipe(inputR, outputW)
	defer engine.Close()
	wire := operatortransport.NewCodec(outputR, inputW)
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	// Test operations have their own watchdog; the parent stays much longer than
	// the one-minute session remainder so passing it unchanged cannot satisfy this test.
	watchdog := time.AfterFunc(10*time.Second, func() { _ = engine.Close() })
	defer watchdog.Stop()
	bounded := make(chan bool, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(parent, engine, operator.Hello{Protocol: 1}, func(ctx context.Context, s *Session, p Peer, g operator.Gate) operator.GateResult {
			end, ok := ctx.Deadline()
			bounded <- ok && time.Until(end) <= 61*time.Second
			return operator.GateResult{Gate: g, Status: operator.Inconclusive, Reason: "precondition-failed"}
		})
	}()
	if f, err := wire.Read(); err != nil || f.Type != "hello" {
		t.Fatal(f, err)
	}
	armed := time.Now().Add(-104 * time.Minute).UnixMilli()
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: decimal(armed), EndUnixMS: decimal(armed + SessionMaximum.Milliseconds())}
	if err := wire.Write(operator.Frame{Type: "init", Session: "session", Init: &init}); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := wire.Read()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == "final" {
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !<-bounded {
		t.Fatal("MUTATION: runner-context-cutoff")
	}
}
