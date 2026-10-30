package controlplane

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/mediaworker"
)

// DialOut runs the reconnect loop until ctx ends. Heartbeats use a separate
// HTTP connection and survive every link reset. Ready closes on first admission.
type DialOut struct {
	URL, Name string
	Worker    *mediaworker.Worker
	Handler   http.Handler
	Ready     chan struct{}
	instance  string
	cycle     atomic.Uint64
	sequence  uint64
	started   bool
	connected bool
}

func (d *DialOut) Run(ctx context.Context) error {
	d.instance = randomToken()
	transport := workerlink.Transport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	delay := time.Duration(0)
	for ctx.Err() == nil {
		if delay > 0 {
			timer := time.NewTimer(time.Duration(rand.Int64N(int64(delay))))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
		d.connected = false
		_ = d.connect(ctx, client)
		if d.connected {
			delay = 0
		} else if delay == 0 {
			delay = 50 * time.Millisecond
		} else {
			delay = min(delay*2, time.Second)
		}
	}

	return nil
}
func (d *DialOut) connect(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL+"/private/workers/"+url.PathEscape(d.Name)+"/link", reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	response := make(chan struct {
		resp *http.Response
		err  error
	}, 1)
	go func() {
		resp, err := client.Do(req)
		response <- struct {
			resp *http.Response
			err  error
		}{resp, err}
	}()
	d.sequence++
	hello := LinkHello{Instance: d.instance, Sequence: d.sequence, Release: buildinfo.Version, Contract: workerlink.ContractVersion, DeathCycle: d.cycle.Load(), Status: d.Worker.StatusSnapshot()}
	// Bound the entire handshake, including an unreachable relay. Canceling the
	// timer after readiness leaves the long-lived stream with no command deadline.
	timer := time.AfterFunc(privateapi.Timeout, cancel)
	defer timer.Stop()
	if err := workerlink.WriteValue(writer, "hello", hello); err != nil {
		return err
	}
	var resp *http.Response
	select {
	case result := <-response:
		if result.err != nil {
			return result.err
		}
		resp = result.resp
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		return errors.New("workerlink: admission refused")
	}
	var welcome LinkWelcome
	if err := workerlink.ReadValue(resp.Body, "welcome", &welcome); err != nil {
		return err
	}
	d.cycle.Store(welcome.DeathCycle)
	address, err := d.Worker.BindRelay(ctx, welcome.Token)
	if err != nil {
		return err
	}
	if err := workerlink.WriteValue(writer, "address", LinkAddress{Address: address}); err != nil {
		return err
	}
	var ready LinkReady
	if err := workerlink.ReadValue(resp.Body, "ready", &ready); err != nil {
		return err
	}
	if err := d.Worker.AdoptRelayAddress(ready.Address); err != nil {
		return err
	}
	timer.Stop()
	d.connected = true
	if !d.started {
		d.Worker.StartHeartbeats(&RemoteHeartbeats{URL: d.URL, Name: d.Name, cycle: &d.cycle})
		d.started = true
		if d.Ready != nil {
			close(d.Ready)
		}
	}
	handler := d.Handler
	if handler == nil {
		handler = d.Worker.PrivateHandler()
	}
	peer := workerlink.New(resp.Body, writer, cancel, d.Worker.LinkHandler(handler))
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); d.watchLeg(ctx, peer, address, ready) }()
	err = peer.Run(ctx)
	cancel()
	<-monitorDone
	return err
}

func (d *DialOut) watchLeg(ctx context.Context, peer *workerlink.Peer, bound netip.AddrPort, ready LinkReady) {
	client := &http.Client{Transport: peer, Timeout: privateapi.Timeout}
	if ready.Activate {
		if err := privateapi.Do(ctx, client, "http://control", http.MethodPost, "/leg-ready", LinkAddress{Address: ready.Address}, nil, nil); err != nil {
			d.connected = false
			peer.Close()
			return
		}
	}
	for {
		if bound != d.Worker.LocalAddr() {
			if err := privateapi.Do(ctx, client, "http://control", http.MethodPost, "/leg-address", LinkAddress{Address: bound}, nil, nil); err == nil {
				peer.Close()
				return
			}
		}
		var err error
		bound, err = d.Worker.WaitRelayObservation(ctx)
		if err != nil {
			return
		}
	}
}
