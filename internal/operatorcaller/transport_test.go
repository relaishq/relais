package operatorcaller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

type mailboxPeer struct {
	requests []operator.Frame
	reply    func(operator.Frame) (operator.Frame, error)
}

func TestProtocolMailboxBoundsRequestByCallerDeadline(t *testing.T) {
	end := time.Now().Add(3 * time.Second).Truncate(time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), end)
	defer cancel()
	peer := &mailboxPeer{reply: func(f operator.Frame) (operator.Frame, error) {
		if f.Request.DeadlineUnixMS != strconv.FormatInt(end.UnixMilli(), 10) || f.Request.RemainingMS > 3000 {
			t.Fatal("mailbox request outlived caller deadline", f.Request)
		}
		zero := 0
		return operator.Frame{Type: "result", Session: f.Session, Result: &operator.Reply{ID: f.Request.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: json.RawMessage(`{"found":false}`)}}, nil
	}}
	box := &ProtocolMailbox{Peer: peer, Session: "caller", End: time.Now().Add(20 * time.Minute)}
	if _, err := box.Read(ctx, "scope/transport.json"); err != nil {
		t.Fatal(err)
	}
}

func (p *mailboxPeer) Send(_ context.Context, f operator.Frame) error {
	p.requests = append(p.requests, f)
	return nil
}
func (p *mailboxPeer) Receive(context.Context) (operator.Frame, error) {
	return p.reply(p.requests[len(p.requests)-1])
}
func TestProtocolMailboxLargeBlobAndLostCreate(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	peer := &mailboxPeer{}
	box := &ProtocolMailbox{Peer: peer, Session: "caller", Spool: root, End: time.Now().Add(time.Minute)}
	data := []byte(strings.Repeat("evidence", 1<<16))
	var saved *operator.Blob
	peer.reply = func(f operator.Frame) (operator.Frame, error) {
		var args MailboxArgs
		if err := json.Unmarshal(f.Request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if f.Request.Operation == operator.MailboxCreate {
			saved = args.Blob
			if args.Blob == nil || len(args.Data) != 0 {
				t.Fatal("large create not spooled")
			}
			raw, err := operatortransport.ReadBlob(root, *args.Blob, operator.MaxBlobBytes)
			if err != nil || string(raw) != string(data) {
				t.Fatal("blob changed", err)
			}
		}
		value := MailboxValue{State: Committed}
		if f.Request.Operation == operator.MailboxRead {
			found := true
			value = MailboxValue{Found: &found, Blob: saved}
		}
		raw, _ := json.Marshal(value)
		zero := 0
		return operator.Frame{Type: "result", Session: f.Session, Result: &operator.Reply{ID: f.Request.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: raw}}, nil
	}
	if box.Create(context.Background(), "scope/report.jsonl", data) != Committed {
		t.Fatal("large create")
	}
	read, err := box.Read(context.Background(), "scope/report.jsonl")
	if err != nil || !read.Found || string(read.Data) != string(data) {
		t.Fatal("large read", err)
	}
	another := &ProtocolMailbox{Peer: peer, Session: "caller", Spool: root, End: time.Now().Add(time.Minute)}
	if another.Create(context.Background(), "other/report.jsonl", data) != Committed {
		t.Fatal("cross-scope blob collision")
	}
	peer.reply = func(operator.Frame) (operator.Frame, error) { return operator.Frame{}, errors.New("lost reply") }
	if box.Create(context.Background(), "scope/feed.json", []byte("one")) != CreateUnknown {
		t.Fatal("lost create")
	}
	n := len(peer.requests)
	if box.Create(context.Background(), "scope/feed.json", []byte("one")) != CreateUnknown || len(peer.requests) != n {
		t.Fatal("MUTATION: mailbox-create-replay")
	}
}

func TestDirectoryMailboxThroughFramedPipes(t *testing.T) {
	toServerR, toServerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	toClientR, toClientW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	client := operatortransport.NewPipe(toClientR, toServerW)
	server := operatortransport.NewPipe(toServerR, toClientW)
	defer client.Close()
	defer server.Close()
	directory, err := OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		for range 2 {
			frame, e := server.Receive(ctx)
			if e != nil {
				served <- e
				return
			}
			var args MailboxArgs
			if e = json.Unmarshal(frame.Request.Args, &args); e != nil {
				served <- e
				return
			}
			requestCtx, stop, e := operatortransport.RequestContext(ctx, *frame.Request)
			if e != nil {
				served <- e
				return
			}
			value := MailboxValue{}
			if frame.Request.Operation == operator.MailboxCreate {
				value.State = directory.Create(requestCtx, args.Key, args.Data)
			} else {
				read, e := directory.Read(requestCtx, args.Key)
				if e != nil {
					stop()
					served <- e
					return
				}
				value.Found = &read.Found
				value.Data = read.Data
			}
			stop()
			raw, _ := json.Marshal(value)
			zero := 0
			if e = server.Send(ctx, operator.Frame{Type: "result", Session: frame.Session, Result: &operator.Reply{ID: frame.Request.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: raw}}); e != nil {
				served <- e
				return
			}
		}
		served <- nil
	}()
	box := &ProtocolMailbox{Peer: client, Session: "caller", End: time.Now().Add(time.Minute)}
	if state := box.Create(ctx, "scope/feed/00000.json", []byte(`{"kind":"probe"}`)); state != Committed {
		t.Fatal(state)
	}
	read, err := box.Read(ctx, "scope/feed/00000.json")
	if err != nil || !read.Found || string(read.Data) != `{"kind":"probe"}` {
		t.Fatal(read, err)
	}
	if err = <-served; err != nil {
		t.Fatal(err)
	}
}
