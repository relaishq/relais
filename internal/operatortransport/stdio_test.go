package operatortransport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestFrameBoundaries(t *testing.T) {
	good := `{"type":"result","session":"s","result":{"id":"1","transport":"unknown"}}` + "\n"
	for _, tt := range []struct {
		name, line string
		valid      bool
	}{
		{"valid", good, true}, {"partial", strings.TrimSpace(good), false}, {"oversize", strings.Repeat("x", operator.MaxFrameBytes) + "\n", false},
		{"unknown-field", strings.Replace(good, `"id":"1"`, `"id":"1","surprise":true`, 1), false},
		{"two-values", strings.TrimSpace(good) + " {}\n", false}, {"invalid-certainty", strings.Replace(good, "unknown", "maybe", 1), false},
		{"wrong-body", strings.Replace(good, `"type":"result"`, `"type":"init"`, 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewCodec(strings.NewReader(tt.line), io.Discard).Read()
			if (err == nil) != tt.valid {
				t.Fatalf("frame valid=%v err=%v", tt.valid, err)
			}
		})
	}
	if _, err := NewCodec(strings.NewReader(""), io.Discard).Read(); err != io.EOF {
		t.Fatal("EOF changed")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestShortWrite(t *testing.T) {
	err := NewCodec(strings.NewReader(""), shortWriter{}).Write(operator.Frame{Type: "hello", Hello: &operator.Hello{Protocol: 1}})
	if err != io.ErrShortWrite {
		t.Fatal(err)
	}
}
func TestPipeRoundTripAndCancel(t *testing.T) {
	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outputR, outputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := NewPipe(inputR, outputW)
	defer p.Close()
	defer inputW.Close()
	defer outputR.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		sent <- NewCodec(nil, inputW).Write(operator.Frame{Type: "hello", Hello: &operator.Hello{Protocol: 1}})
	}()
	frame, err := p.Receive(ctx)
	if err != nil || frame.Type != "hello" {
		t.Fatal(frame, err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err = p.Receive(stopped); err == nil {
		t.Fatal("cancel did not close wait")
	}
}
func TestBoundedBlobs(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	data := []byte("{\"value\":1}\n")
	sum := sha256.Sum256(data)
	ref := operator.Blob{Name: "payload", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(filepath.Join(dir, ref.Name), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBlob(root, ref, 100)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	for _, bad := range []operator.Blob{{Name: "../payload", Size: ref.Size, SHA256: ref.SHA256}, {Name: ref.Name, Size: ref.Size + 1, SHA256: ref.SHA256}, {Name: ref.Name, Size: ref.Size, SHA256: strings.Repeat("0", 64)}} {
		if _, err := ReadBlob(root, bad, 100); err == nil {
			t.Fatal("bad blob accepted")
		}
	}
	if _, err := ReadBlob(root, ref, 1); err == nil {
		t.Fatal("oversize accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	ref.Name = "escape"
	if _, err := ReadBlob(root, ref, 100); err == nil {
		t.Fatal("spool escape accepted")
	}
}
func TestAdapterHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "adapter-helper" {
		return
	}
	if os.Getenv("OPERATOR_TEST_CREDENTIAL") != "" {
		os.Exit(3)
	}
	codec := NewCodec(os.Stdin, os.Stdout)
	f, err := codec.Read()
	if err != nil {
		os.Exit(4)
	}
	if err := codec.Write(f); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}
func TestSubprocessAdapter(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPERATOR_TEST_CREDENTIAL", "must-not-reach-child")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child, err := Start(ctx, executable, []string{"-test.run=^TestAdapterHelper$", "adapter-helper"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	f := operator.Frame{Type: "hello", Hello: &operator.Hello{Protocol: 1}}
	if err := child.Send(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := child.Receive(ctx)
	if err != nil || got.Type != f.Type {
		t.Fatal(got, err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
}

func transportRequest(id string) operator.Frame {
	return operator.Frame{Type: "request", Session: "s", Request: &operator.Request{ID: id, Operation: operator.HostExecute, Target: "shared", Action: "move", DeadlineUnixMS: strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10), RemainingMS: 60000}}
}
func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("barrier not reached")
	}
}

// The encoder barrier fixes the exact interleaving: Poll saw no event, encoding
// paused, the reader published a fault, then the writer attempted admission.
func TestFaultPublicationFencesSubmission(t *testing.T) {
	for _, fault := range []string{"duplicate", "malformed", "eof", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			inR, inW := io.Pipe()
			out := &countingWriter{}
			entered, release := make(chan struct{}), make(chan struct{})
			p := newPipe(inR, out, func(f operator.Frame) ([]byte, error) { close(entered); <-release; return encodeFrame(f) })
			defer p.Close()
			defer inW.Close()
			p.admission.Lock()
			p.expected["s/1"] = true
			p.admission.Unlock()
			if _, _, ready := p.Poll(); ready {
				t.Fatal("inbox initially ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sent := make(chan error, 1)
			go func() { sent <- p.Send(ctx, transportRequest("2")) }()
			waitSignal(t, entered)
			switch fault {
			case "duplicate":
				if err := NewCodec(nil, inW).Write(operator.Frame{Type: "result", Session: "s", Result: &operator.Reply{ID: "1", Transport: operator.Succeeded}}); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if _, err := io.WriteString(inW, "{broken}\n"); err != nil {
					t.Fatal(err)
				}
			case "eof":
				_ = inW.Close()
			case "cancel":
				if err := NewCodec(nil, inW).Write(operator.Frame{Type: "cancel", Code: "interrupted"}); err != nil {
					t.Fatal(err)
				}
			}
			waitSignal(t, p.faulted)
			close(release)
			if err := <-sent; err == nil || out.calls.Load() != 0 {
				t.Fatal("MUTATION: publication-fence")
			}
		})
	}
}

type countingWriter struct{ calls atomic.Int32 }

func (w *countingWriter) Write(b []byte) (int, error) { w.calls.Add(1); return len(b), nil }
func (w *countingWriter) Close() error                { return nil }

func TestDeadlineAfterEncodingBeforeWrite(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &countingWriter{}
	entered, release := make(chan struct{}), make(chan struct{})
	p := newPipe(inR, out, func(f operator.Frame) ([]byte, error) {
		b, err := encodeFrame(f)
		close(entered)
		<-release
		return b, err
	})
	defer p.Close()
	now := time.Now()
	var expiredClock atomic.Bool
	p.now = func() time.Time {
		if expiredClock.Load() {
			return now.Add(2 * time.Minute)
		}
		return now
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() { sent <- p.Send(ctx, transportRequest("1")) }()
	waitSignal(t, entered)
	expiredClock.Store(true)
	close(release)
	if err := <-sent; !errors.Is(err, ErrExpired) || out.calls.Load() != 0 {
		t.Fatal("MUTATION: writer-deadline")
	}
}

func TestAdapterRefusesExpiredQueuedRequest(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := NewPipe(inR, outW)
	defer p.Close()
	defer inW.Close()
	defer outR.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := transportRequest("1")
	request.Request.DeadlineUnixMS = "1"
	wrote := make(chan error, 1)
	go func() { wrote <- NewCodec(nil, inW).Write(request) }()
	refused := make(chan operator.Frame, 1)
	go func() { f, _ := NewCodec(outR, nil).Read(); refused <- f }()
	if _, err := p.Receive(ctx); !errors.Is(err, ErrExpired) {
		t.Fatal("MUTATION: adapter-expiry")
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	f := <-refused
	if f.Result == nil || f.Result.Transport != operator.Failed || !f.Result.NotSubmitted || f.Result.RefusalCode != "deadline-before-submission" {
		t.Fatal("expired request lacks proved refusal")
	}
}

func TestDuplicateInboundRequestNeverDelivered(t *testing.T) {
	for _, mode := range []string{"already-executed", "queued-before-fault"} {
		t.Run(mode, func(t *testing.T) {
			inputR, inputW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer inputW.Close()
			out := &countingWriter{}
			p := NewPipe(inputR, out)
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			codec := NewCodec(nil, inputW)
			first := transportRequest("1")
			executed := 0
			if err := codec.Write(first); err != nil {
				t.Fatal(err)
			}
			if mode == "already-executed" {
				f, err := p.Receive(ctx)
				if err != nil || f.Request == nil {
					t.Fatal("first request not admitted", err)
				}
				executed++ // The adapter's effect seam executes only a successfully delivered request.
			}
			duplicate, _ := encodeFrame(first)
			successor, _ := encodeFrame(transportRequest("2"))
			if _, err := inputW.Write(append(duplicate, successor...)); err != nil {
				t.Fatal(err)
			}
			if mode == "queued-before-fault" {
				waitSignal(t, p.Faulted())
			}
			f, err := p.Receive(ctx)
			if err == nil && f.Request != nil {
				executed++
			}
			want := 0
			if mode == "already-executed" {
				want = 1
			}
			if err == nil || f.Request != nil || executed != want {
				t.Fatal("MUTATION: duplicate-inbound-delivery")
			}
			// The terminal reader must reject later delivery without waiting for another
			// frame, even though a fresh ID was already written after the duplicate.
			f, err = p.Receive(ctx)
			if err == nil || f.Request != nil {
				t.Fatal("post-fault request reached execution")
			}
			if err := p.Send(ctx, transportRequest("3")); err == nil || out.calls.Load() != 0 {
				t.Fatal("post-fault effect submission escaped")
			}
		})
	}
}
func TestPublishedFaultRejectsPassingFinal(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &countingWriter{}
	p := NewPipe(inR, out)
	defer p.Close()
	if _, err := io.WriteString(inW, "{malformed}\n"); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, p.Faulted())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := operator.Frame{Type: "final", Session: "s", Final: &operator.Result{Status: operator.Passed, Complete: true}}
	if err := p.Send(ctx, f); err == nil || out.calls.Load() != 0 {
		t.Fatal("MUTATION: passing-final-fence")
	}
}
