package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/privateapi"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// RemoteWorker implements Worker with bounded HTTP requests. No mutation is
// automatically retried. Supply Client to override the two-second default.
type RemoteWorker struct {
	URL               string
	Client            *http.Client
	diagnosticContext context.Context
}

// ErrCreateNotSent proves the worker could not have accepted this request.
var ErrCreateNotSent = errors.New("controlplane: create request not sent")

// RemoteRelay implements Relay and exposes registry operations to bootstrap.
type RemoteRelay struct {
	URL               string
	Client            *http.Client
	diagnosticContext context.Context
}

func remoteClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: privateapi.Timeout}
}

func (w *RemoteWorker) CreateSession(ctx context.Context, offer string) (string, string, error) {
	id, answer, _, err := w.CreateSessionWithLease(ctx, offer)
	return id, answer, err
}
func (w *RemoteWorker) CreateSessionWithLease(ctx context.Context, offer string) (string, string, sessionstore.Lease, error) {
	if err := ctx.Err(); err != nil {
		return "", "", sessionstore.Lease{}, errors.Join(ErrCreateNotSent, err)
	}
	var reply mediaworker.CreateReply
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/sessions", mediaworker.CreateRequest{Offer: offer}, &reply, mediaworker.RemoteErrors)
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		err = errors.Join(ErrCreateNotSent, dial)
	}
	return reply.ID, reply.Answer, reply.Lease, err
}
func (w *RemoteWorker) EndSession(id string) error {
	return privateapi.Do(context.Background(), remoteClient(w.Client), w.URL, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil, mediaworker.RemoteErrors)
}
func (w *RemoteWorker) ExportSession(id string) ([]byte, error) {
	var reply mediaworker.ExportReply
	err := privateapi.Do(movediag.Copy(context.Background(), w.diagnosticContext), remoteClient(w.Client), w.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/export", nil, &reply, mediaworker.RemoteErrors)
	return reply.State, err
}
func (w *RemoteWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var reply mediaworker.ResumeReply
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/resume", mediaworker.ResumeRequest{Kind: opts.Kind, InputMayBeDuplicated: opts.InputMayBeDuplicated, DuplicateWindows: opts.DuplicateWindows, CallerSequenceReserve: opts.CallerSequenceReserve, CheckpointAge: opts.CheckpointAge, SnapshotAge: opts.SnapshotAge, CheckpointStoredAt: opts.CheckpointStoredAt, State: state, Lease: opts.Lease, SequenceMargin: opts.SequenceMargin, SRTCPIndexMargin: opts.SRTCPIndexMargin, RelayReplay: opts.RelayReplay, RelayInputReplay: opts.RelayInputReplay}, &reply, mediaworker.RemoteErrors)
	return reply.ID, err
}
func (w *RemoteWorker) RequestKeyframe(ctx context.Context, id string) error {
	return privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/keyframe", nil, nil, mediaworker.RemoteErrors)
}
func (w *RemoteWorker) Status(ctx context.Context) (mediaworker.WorkerStatus, error) {
	var reply mediaworker.WorkerStatus
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodGet, "/status", nil, &reply, mediaworker.RemoteErrors)
	return reply, err
}
func (r *RemoteRelay) route(ctx context.Context, id, action string, from, to netip.AddrPort, output any) error {
	return privateapi.Do(movediag.Copy(ctx, r.diagnosticContext), remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/"+action, relay.RouteRequest{From: from, To: to, Repair: action == "move" && !from.IsValid()}, output, relay.RemoteErrors)
}
func (r *RemoteRelay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	return r.route(ctx, id, "hold", from, netip.AddrPort{}, nil)
}
func (r *RemoteRelay) MoveSession(id string, from, to netip.AddrPort) error {
	return r.route(context.Background(), id, "move", from, to, nil)
}
func (r *RemoteRelay) MoveSessionAtEpoch(id string, from, to netip.AddrPort, epoch uint64) error {
	return privateapi.Do(context.Background(), remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/move", relay.RouteRequest{From: from, To: to, Repair: !from.IsValid(), Epoch: epoch}, nil, relay.RemoteErrors)
}
func (r *RemoteRelay) ReleaseSession(id string, to netip.AddrPort) (int, error) {
	var reply relay.ReleaseReply
	err := r.route(context.Background(), id, "release", netip.AddrPort{}, to, &reply)
	return reply.Packets, err
}

// MoveSessionAfterExport uses the ADR 0009 route endpoint, falling back to
// the existing /move only when an older relay's mux answers 404 or 405.
func (r *RemoteRelay) MoveSessionAfterExport(id string, from, to netip.AddrPort, epoch uint64) error {
	if !from.IsValid() {
		// The ADR 0009 route needs a source to drain; this is a route repair.
		return r.MoveSessionAtEpoch(id, from, to, epoch)
	}
	err := privateapi.Do(movediag.Copy(context.Background(), r.diagnosticContext), remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/move-after-export", relay.RouteRequest{From: from, To: to, Epoch: epoch}, nil, relay.RemoteErrors)
	if olderRelay(err) {
		return r.MoveSessionAtEpoch(id, from, to, epoch)
	}
	return err
}

func olderRelay(err error) bool {
	var response *privateapi.HTTPError
	return errors.As(err, &response) && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed)
}

// ReleaseAfterExport uses the ADR 0009 endpoint. A relay without that route
// answers 404 or 405 from its mux (domain errors are 409), and only then is
// the hold released with the existing /release, which keeps the old limit.
// Uncertain and other errors are returned, never retried.
func (r *RemoteRelay) ReleaseAfterExport(id string, to netip.AddrPort, input map[uint32]relay.ConsumedInput) (relay.ReleaseReply, error) {
	var reply relay.ReleaseReply
	err := privateapi.Do(movediag.Copy(context.Background(), r.diagnosticContext), remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/release-after-export", relay.ExportReleaseRequest{To: to, Input: input}, &reply, relay.RemoteErrors)
	if olderRelay(err) {
		held, err := r.ReleaseSession(id, to)
		return relay.ReleaseReply{Packets: held}, err
	}
	return reply, err
}

func (r *RemoteRelay) BeginReplay(ctx context.Context, id string, from netip.AddrPort, inbound map[uint32]uint64) (relay.ReplayPlan, error) {
	var reply relay.ReplayPlan
	err := privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/begin-replay", relay.ReplayRequest{From: from, Inbound: inbound}, &reply, relay.RemoteErrors)
	return reply, err
}

func (r *RemoteRelay) ReplaySession(ctx context.Context, id string, to netip.AddrPort) (relay.ReplayResult, error) {
	var reply relay.ReplayResult
	err := privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/replay", relay.ReplayRequest{To: to}, &reply, relay.RemoteErrors)
	return reply, err
}

// ForgetSession cannot report errors through Relay's existing interface.
// Forget returns the transport result for applications that need it.
func (r *RemoteRelay) ForgetSession(id string) { _ = r.Forget(context.Background(), id) }
func (r *RemoteRelay) ForgetSessionTenure(ctx context.Context, lease sessionstore.Lease) error {
	err := privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(lease.SessionID)+"/forget-tenure", lease, nil, relay.RemoteErrors)
	var response *privateapi.HTTPError
	if errors.As(err, &response) && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed) {
		slog.Warn("relay tenure cleanup unavailable; skipping older relay", "status", response.StatusCode)
		return nil
	}
	return err
}
func (r *RemoteRelay) Forget(ctx context.Context, id string) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil, relay.RemoteErrors)
}
func (r *RemoteRelay) AddWorker(ctx context.Context, addr netip.AddrPort) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/workers", relay.WorkerRequest{Address: addr}, nil, relay.RemoteErrors)
}
func (r *RemoteRelay) RemoveWorker(ctx context.Context, addr netip.AddrPort) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodDelete, "/workers", relay.WorkerRequest{Address: addr}, nil, relay.RemoteErrors)
}
func (r *RemoteRelay) Status(ctx context.Context) (relay.RelayStatus, error) {
	var reply relay.RelayStatus
	err := privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodGet, "/status", nil, &reply, relay.RemoteErrors)
	return reply, err
}

// RemoteHeartbeats adapts the existing worker fence/drop sequence to an
// explicit token acknowledgement. Only the call after ErrRejoinRequired sends
// the token, so receiving a challenge is never itself an acknowledgement.
// The worker serializes calls to Heartbeat.
type RemoteHeartbeats struct {
	URL, Name string
	Client    *http.Client
	token     string
}

func (h *RemoteHeartbeats) Heartbeat(addr netip.AddrPort) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var reply HeartbeatReply
	err := privateapi.Do(ctx, remoteClient(h.Client), h.URL, http.MethodPost, "/private/workers/"+url.PathEscape(h.Name)+"/heartbeat", HeartbeatRequest{Address: addr, Token: h.token}, &reply, nil)
	if err != nil {
		return err
	}
	if reply.Token != "" {
		h.token = reply.Token
		return mediaworker.ErrRejoinRequired
	}
	h.token = ""
	return nil
}

func diagnosticWorker(w Worker, ctx context.Context) Worker {
	if remote, ok := w.(*RemoteWorker); ok && movediag.ID(ctx) != "" {
		copy := *remote
		copy.diagnosticContext = ctx
		return &copy
	}
	return w
}
func diagnosticRelay(r Relay, ctx context.Context) Relay {
	if remote, ok := r.(*RemoteRelay); ok && movediag.ID(ctx) != "" {
		copy := *remote
		copy.diagnosticContext = ctx
		return &copy
	}
	return r
}

func (r *RemoteWorker) AgentControl(ctx context.Context, id string, message agentv2.ControlMessage) (agentv2.ControlReceipt, error) {
	var receipt agentv2.ControlReceipt
	err := privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/agent/control", message, &receipt, mediaworker.RemoteErrors)
	return receipt, err
}
