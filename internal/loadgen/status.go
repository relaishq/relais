package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/relais/pkg/controlplane"
)

// statusPoller coalesces callers' existing one-second polls and final reads.
// Both successes and errors are cached: an outage must not cause N retries.
// It refreshes on demand, under the harness's observer/move exclusion lock.
type statusPoller struct {
	mu       sync.Mutex
	endpoint string
	client   *http.Client
	meter    *Meter
	every    time.Duration
	now      func() time.Time
	fetched  time.Time
	status   controlplane.Status
	err      error
}

func newStatusPoller(signaling string, meter *Meter) *statusPoller {
	return &statusPoller{endpoint: strings.TrimSuffix(signaling, "/calls") + "/status", client: &http.Client{Timeout: time.Second}, meter: meter, every: time.Second, now: time.Now}
}

func (p *statusPoller) Get(ctx context.Context) (controlplane.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fetched.IsZero() && p.now().Sub(p.fetched) < p.every {
		return p.status, p.err
	}
	p.fetched = p.now()
	p.status, p.err = p.fetch(ctx)
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
