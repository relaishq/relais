package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
)

// statusPoller coalesces callers' existing one-second polls. Final reads
// bypass periodic snapshots, sharing one fresh final snapshot for the batch.
// Requests use their own deadlines; a caller's cancellation is never cached.
type statusPoller struct {
	mu                        sync.Mutex
	endpoint                  string
	client                    *http.Client
	meter                     *Meter
	every                     time.Duration
	now                       func() time.Time
	fetched                   time.Time
	status                    controlplane.Status
	err                       error
	final                     bool
	pollTimeout, finalTimeout time.Duration
}

func newStatusPoller(signaling string, meter *Meter, client *http.Client) *statusPoller {
	if client == nil {
		client = &http.Client{}
	}
	return &statusPoller{endpoint: strings.TrimSuffix(signaling, "/calls") + "/status", client: client, meter: meter, every: time.Second, now: time.Now, pollTimeout: time.Second, finalTimeout: 10 * time.Second}
}

func (p *statusPoller) Get(ctx context.Context) (controlplane.Status, error) {
	return p.get(ctx, false)
}

func (p *statusPoller) GetFinal(ctx context.Context) (controlplane.Status, error) {
	return p.get(ctx, true)
}

func (p *statusPoller) get(ctx context.Context, final bool) (controlplane.Status, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Status{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return controlplane.Status{}, err
	}
	if !p.fetched.IsZero() && p.now().Sub(p.fetched) < p.every && (!final || p.final) {
		return p.status, p.err
	}
	timeout := p.pollTimeout
	if final {
		timeout = p.finalTimeout
	}
	owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	p.status, p.err = p.fetch(owned)
	// Cache from completion so concurrent readers share even a slow result.
	p.fetched, p.final = p.now(), final
	// This error belongs to this reader only, not to the shared snapshot.
	if err := ctx.Err(); err != nil {
		return controlplane.Status{}, err
	}
	return p.status, p.err
}

func (p *statusPoller) fetch(ctx context.Context) (controlplane.Status, error) {
	var status controlplane.Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint, nil)
	if err != nil {
		return status, err
	}
	p.meter.observeStatusRequest()
	resp, err := p.client.Do(req)
	if err != nil {
		return status, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("status poll: %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&status)
	return status, err
}
