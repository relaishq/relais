package controlplane

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
)

// RemoteWorker implements Worker with bounded HTTP requests. No mutation is
// automatically retried. Supply Client to override the two-second default.
type RemoteWorker struct {
	URL    string
	Client *http.Client
}

// RemoteRelay implements Relay and exposes registry operations to bootstrap.
type RemoteRelay struct {
	URL    string
	Client *http.Client
}

func remoteClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: privateapi.Timeout}
}

func (w *RemoteWorker) CreateSession(ctx context.Context, offer string) (string, string, error) {
	var reply mediaworker.CreateReply
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/sessions", mediaworker.CreateRequest{Offer: offer}, &reply, mediaworker.RemoteErrors)
	return reply.ID, reply.Answer, err
}
func (w *RemoteWorker) EndSession(id string) error {
	return privateapi.Do(context.Background(), remoteClient(w.Client), w.URL, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil, mediaworker.RemoteErrors)
}
func (w *RemoteWorker) ExportSession(id string) ([]byte, error) {
	var reply mediaworker.ExportReply
	err := privateapi.Do(context.Background(), remoteClient(w.Client), w.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/export", nil, &reply, mediaworker.RemoteErrors)
	return reply.State, err
}
func (w *RemoteWorker) ResumeSession(state []byte, opts mediaworker.ResumeOptions) (string, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var reply mediaworker.ResumeReply
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/resume", mediaworker.ResumeRequest{CallerSequenceReserve: opts.CallerSequenceReserve, CheckpointAge: opts.CheckpointAge, SnapshotAge: opts.SnapshotAge, CheckpointStoredAt: opts.CheckpointStoredAt, State: state, Lease: opts.Lease, SequenceMargin: opts.SequenceMargin, SRTCPIndexMargin: opts.SRTCPIndexMargin}, &reply, mediaworker.RemoteErrors)
	return reply.ID, err
}
func (w *RemoteWorker) Status(ctx context.Context) (mediaworker.WorkerStatus, error) {
	var reply mediaworker.WorkerStatus
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodGet, "/status", nil, &reply, mediaworker.RemoteErrors)
	return reply, err
}
func (r *RemoteRelay) route(ctx context.Context, id, action string, from, to netip.AddrPort, output any) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/"+action, relay.RouteRequest{From: from, To: to, Repair: action == "move" && !from.IsValid()}, output, relay.RemoteErrors)
}
func (r *RemoteRelay) HoldSession(ctx context.Context, id string, from netip.AddrPort) error {
	return r.route(ctx, id, "hold", from, netip.AddrPort{}, nil)
}
func (r *RemoteRelay) MoveSession(id string, from, to netip.AddrPort) error {
	return r.route(context.Background(), id, "move", from, to, nil)
}
func (r *RemoteRelay) ReleaseSession(id string, to netip.AddrPort) (int, error) {
	var reply relay.ReleaseReply
	err := r.route(context.Background(), id, "release", netip.AddrPort{}, to, &reply)
	return reply.Packets, err
}

// ForgetSession cannot report errors through Relay's existing interface.
// Forget returns the transport result for applications that need it.
func (r *RemoteRelay) ForgetSession(id string) { _ = r.Forget(context.Background(), id) }
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
