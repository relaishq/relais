package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
)

// AgentSamples are private, sampled current-age evidence, separate from resource
// scrapes and from the checkpoint-age histogram measured only at takeover.
type AgentSample struct {
	At     time.Time                      `json:"at"`
	Worker string                         `json:"worker"`
	Status mediaworker.AgentSessionStatus `json:"status"`
}
type AgentSamples struct {
	IntervalMS int           `json:"interval_ms"`
	Requests   uint64        `json:"private_http_requests"`
	Missing    uint64        `json:"missing_session_samples"`
	Samples    []AgentSample `json:"samples,omitempty"`
	Errors     []string      `json:"errors,omitempty"`
}

func (m *Monitor) sampleAgents(ctx context.Context) {
	// Probe a remembered owner first. Moves invalidate it with unknown_session;
	// search only then. No Redis reads or application callbacks are performed.
	owners := map[string]int{}
	ids := map[string]bool{}
	for _, target := range m.targets {
		for _, id := range target.AgentSessions {
			ids[id] = true
		}
	}
	m.data.Agents.IntervalMS = 100
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		for id := range ids {
			found := false
			first, known := owners[id]
			order := []int{}
			if known {
				order = append(order, first)
			}
			for i, t := range m.targets {
				if strings.HasPrefix(t.Name, "worker-") && (!known || i != first) {
					order = append(order, i)
				}
			}
			for _, i := range order {
				if m.retired[i].Load() {
					continue
				}
				t := m.targets[i]
				endpoint := t.URL + "/status?agent-session=" + url.QueryEscape(id)
				m.data.Agents.Requests++
				status, code, err := readAgentStatus(ctx, m.client, endpoint)
				// A deliberate SIGKILL can race an in-flight request. Its result
				// belongs to the retired owner and cannot invalidate the new owner.
				if m.retired[i].Load() {
					continue
				}
				if ctx.Err() != nil {
					return
				}
				if code == http.StatusConflict {
					continue
				}
				if err != nil {
					m.data.Agents.Errors = append(m.data.Agents.Errors, t.Name+": "+err.Error())
					continue
				}
				if status == nil || status.ID != id || status.Name == "" || !status.CheckpointKnown {
					m.data.Agents.Errors = append(m.data.Agents.Errors, t.Name+": missing custom-agent checkpoint diagnostic")
					continue
				}
				owners[id] = i
				m.data.Agents.Samples = append(m.data.Agents.Samples, AgentSample{At: time.Now(), Worker: t.Name, Status: *status})
				found = true
				break
			}
			if !found {
				m.data.Agents.Missing++
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func readAgentStatus(ctx context.Context, client *http.Client, endpoint string) (*mediaworker.AgentSessionStatus, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("agent status HTTP %d", resp.StatusCode)
	}
	var status mediaworker.WorkerStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status); err != nil {
		return nil, resp.StatusCode, err
	}
	return status.AgentSession, resp.StatusCode, nil
}
