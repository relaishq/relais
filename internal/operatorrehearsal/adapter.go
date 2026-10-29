package operatorrehearsal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// Caller is the minimal P4 seam. Request carries the preallocated identity,
// phase and typed P4 args unchanged. Replies preserve transport and operation
// evidence; the session, rather than this adapter, judges that evidence.
type Caller interface {
	Ready(context.Context, operator.Request) operator.Reply
	Start(context.Context, operator.Request) operator.Reply
	Stop(context.Context, operator.Request) operator.Reply
}

// Mailbox binds P4's create-only directory store without copying its policy.
type Mailbox interface {
	Read(context.Context, operator.Request) operator.Reply
	Create(context.Context, operator.Request) operator.Reply
}

type Adapter struct {
	Topology *Topology
	// Containers is a snapshot from Deployment.ContainerIDs after successful boot.
	Containers  map[string]string
	Runner      Runner
	Caller      Caller
	Mailbox     Mailbox
	Diagnostics io.Writer
	mu          sync.Mutex
	seen        map[string]bool
}

func (a *Adapter) Capabilities() []operator.Operation {
	var ops []operator.Operation
	if a.Topology != nil && a.Runner != nil {
		ops = append(ops, operator.HostExecute)
	}
	if a.Caller != nil {
		ops = append(ops, operator.CallerReady, operator.CallerStart, operator.CallerStop)
	}
	if a.Mailbox != nil {
		ops = append(ops, operator.MailboxRead, operator.MailboxCreate)
	}
	return ops
}

func refusal(id, code string) operator.Reply {
	return operator.Reply{ID: id, Transport: operator.Failed, NotSubmitted: true, RefusalCode: code}
}

// Execute admits one request once. It never repairs, retries or replays a lost
// docker exec. The same method serves both direct and pipe adapters.
func (a *Adapter) Execute(ctx context.Context, req operator.Request) operator.Reply {
	a.mu.Lock()
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	duplicate := a.seen[req.ID]
	a.seen[req.ID] = true
	a.mu.Unlock()
	if duplicate || req.ID == "" {
		return operator.Reply{ID: req.ID, Transport: operator.Unknown}
	}
	bounded, cancel, err := operatortransport.RequestContext(ctx, req)
	if err != nil {
		return refusal(req.ID, "deadline-before-submission")
	}
	defer cancel()
	switch req.Operation {
	case operator.MailboxRead, operator.MailboxCreate:
		if a.Mailbox == nil {
			return refusal(req.ID, "unsupported-operation")
		}
		if req.Operation == operator.MailboxRead {
			return a.Mailbox.Read(bounded, req)
		}
		return a.Mailbox.Create(bounded, req)
	case operator.CallerReady, operator.CallerStart, operator.CallerStop:
		if a.Caller == nil {
			return refusal(req.ID, "unsupported-operation")
		}
		switch req.Operation {
		case operator.CallerReady:
			return a.Caller.Ready(bounded, req)
		case operator.CallerStart:
			return a.Caller.Start(bounded, req)
		default:
			return a.Caller.Stop(bounded, req)
		}
	case operator.HostExecute:
		if a.Topology == nil {
			return refusal(req.ID, "invalid-target")
		}
		if _, ok := a.Topology.Hosts[req.Target]; !ok {
			return refusal(req.ID, "invalid-target")
		}
		id := a.Containers[req.Target]
		if !dockerID.MatchString(id) {
			return refusal(req.ID, "invalid-target")
		}
		if req.Action == "" || len(req.Script) == 0 || len(req.Script) > operator.MaxScriptBytes || a.Runner == nil {
			return refusal(req.ID, "invalid-request")
		}
		if bounded.Err() != nil {
			return refusal(req.ID, "deadline-before-submission")
		}
		deadline, _ := bounded.Deadline()
		seconds := max(1, int(time.Until(deadline).Seconds()))
		// timeout also lives inside the container: killing the docker client alone
		// would not stop the submitted command. Its outcome still remains unknown.
		wrapper := `f=$(mktemp) || exit 125; trap 'rm -f "$f"' EXIT; cat > "$f" || exit 125; timeout -s KILL ` + strconv.Itoa(seconds) + `s nice -n 10 sh "$f"`
		command := Command{Label: "host-step", Args: []string{"docker", "exec", "-i", id, "sh", "-c", wrapper}, Input: req.Script}
		result := a.Runner.Run(bounded, command)
		reply := a.classify(req.ID, result)
		if reply.Transport != operator.Succeeded || hostObservationFailed(req.Action, result.Stdout) {
			a.printFailure(result)
		}
		return reply
	default:
		return refusal(req.ID, "unsupported-operation")
	}
}

// A script can exit zero while its typed envelope refuses or lacks evidence.
// Retain that original output for diagnosis as well as transport failures.
func hostObservationFailed(action string, stdout []byte) bool {
	raw, err := host.Unpack(string(stdout), 256000)
	if err != nil {
		return true
	}
	if action == "inventory" || action == "clock" {
		return false
	}
	var envelope struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal(raw, &envelope) != nil || envelope.Kind != "ok"
}

func (a *Adapter) classify(id string, result CommandResult) operator.Reply {
	r := operator.Reply{ID: id, Transport: operator.Unknown}
	if result.Err != nil || result.Code == nil || *result.Code < 0 || *result.Code == 124 || *result.Code == 125 || *result.Code == 137 || result.Truncated || len(result.Stdout) > operator.MaxHostOutputBytes || len(result.Stderr) > operator.MaxHostOutputBytes || !utf8.Valid(result.Stdout) || !utf8.Valid(result.Stderr) {
		r.UncertaintyRule = "unconfirmed-exit"
		switch {
		case errors.Is(result.Err, context.DeadlineExceeded):
			r.UncertaintyRule = "timeout"
		case errors.Is(result.Err, context.Canceled):
			r.UncertaintyRule = "cancelled"
		case result.Truncated || len(result.Stdout) > operator.MaxHostOutputBytes || len(result.Stderr) > operator.MaxHostOutputBytes:
			r.UncertaintyRule = "truncated-output"
		case !utf8.Valid(result.Stdout) || !utf8.Valid(result.Stderr):
			r.UncertaintyRule = "malformed-reply"
		case result.Code != nil && (*result.Code == 124 || *result.Code == 137):
			r.UncertaintyRule = "timeout"
		case result.Err != nil:
			r.UncertaintyRule = "lost-reply"
		case result.Code == nil:
			r.UncertaintyRule = "missing-exit-status"
		}
		return r
	}
	r.ExitCode = result.Code
	r.Transport = operator.Failed
	if *result.Code == 0 {
		r.Transport = operator.Succeeded
	}
	// Credentials are removed before any byte crosses a pipe or diagnostic sink.
	r.Payload, _ = json.Marshal(operator.HostOutput{Stdout: a.Topology.Redact(string(result.Stdout))})
	if len(r.Payload) > operator.MaxFrameBytes/2 {
		return operator.Reply{ID: id, Transport: operator.Unknown, UncertaintyRule: "truncated-output"}
	}
	return r
}
func (a *Adapter) printFailure(result CommandResult) {
	if a.Diagnostics == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = io.WriteString(a.Diagnostics, "container host-step failed\n"+a.Topology.diagnosticStreams(result))
}

// Serve services the environment half of the P1 JSON Lines protocol. Setup and
// disposable teardown belong to its caller, outside the gate-session latch.
func (a *Adapter) Serve(ctx context.Context, p *operatortransport.Pipe, session string, init operator.Init) (*operator.Result, error) {
	defer p.Close()
	f, err := p.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if f.Type != "hello" {
		return nil, errors.New("expected-hello")
	}
	if err = p.Send(ctx, operator.Frame{Type: "init", Session: session, Init: &init}); err != nil {
		return nil, err
	}
	var traces []operator.Trace
	for {
		f, err = p.Receive(ctx)
		if err != nil {
			return nil, err
		}
		if f.Session != session {
			return nil, errors.New("session-mismatch")
		}
		switch f.Type {
		case "request":
			r := a.Execute(ctx, *f.Request)
			if err = p.Send(ctx, operator.Frame{Type: "result", Session: session, Result: &r}); err != nil {
				return nil, err
			}
		case "trace": // Only the public session builds traces.
			traces = append(traces, *f.Trace)
		case "final":
			if f.Final != nil && (len(f.Final.Trace) < len(traces) || !slices.Equal(f.Final.Trace[:len(traces)], traces)) {
				f.Final.Trace = append(traces, f.Final.Trace...)
			}
			return f.Final, nil
		default:
			return nil, errors.New("unexpected-session-frame")
		}
	}
}

func (t *Topology) Redact(text string) string {
	// Values are equal length today; longest first also protects future overlaps.
	values := make([]string, 0, len(t.Secrets))
	for _, value := range t.Secrets {
		if value != "" {
			values = append(values, value)
		}
	}
	sortSecrets(values)
	for _, value := range values {
		text = replaceSecret(text, value)
	}
	return text
}
