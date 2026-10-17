package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
)

type storeStallAgentEvidence struct {
	Name   string                           `json:"name"`
	Before *mediaworker.AgentSessionStatus  `json:"before"`
	During []mediaworker.AgentSessionStatus `json:"during"`
	After  *mediaworker.AgentSessionStatus  `json:"after,omitempty"`
	Errors []string                         `json:"errors,omitempty"`
}

func (s *storeStallOptions) observeAgent(ctx context.Context, endpoint string) *mediaworker.AgentSessionStatus {
	if s.Agent == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/status?agent-session="+url.QueryEscape(s.SessionID), nil)
	var status mediaworker.WorkerStatus
	if err == nil {
		client := &http.Client{Timeout: 500 * time.Millisecond}
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusConflict && s.Variant == "expired" {
				_ = resp.Body.Close()
				return nil
			}
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(resp.Body).Decode(&status)
			} else {
				err = fmt.Errorf("agent diagnostics HTTP %d", resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		s.Agent.Errors = append(s.Agent.Errors, err.Error())
		return nil
	}
	return status.AgentSession
}
func judgeStoreStallAgent(e *storeStallEvidence) []string {
	a := e.Agent
	if a == nil {
		return nil
	}
	var failures []string
	check := func(ok bool, why string) {
		if !ok {
			failures = append(failures, "custom agent: "+why)
		}
	}
	check(a.Before != nil && a.Before.Name == a.Name && a.Before.CheckpointKnown, "pre-stall checkpoint missing")
	check(len(a.Errors) == 0, "diagnostics failed")
	check(len(a.During) > 0, "no in-outage local diagnostic evidence")
	if a.Before != nil {
		advanced, playback := false, false
		for _, d := range a.During {
			advanced = advanced || d.Progress.Consumed > a.Before.Progress.Consumed
			playback = playback || d.PlaybackPackets > 0
		}
		check(advanced, "local input did not advance during store stall")
		check(playback || e.Variant == "moderate", "local task Emit/playout not observed during store stall")
	}
	if e.Variant == "moderate" {
		check(a.After != nil, "stale takeover diagnostics missing")
		if a.After != nil {
			var state struct {
				Resume *struct {
					MoveOnly string `json:"move_only"`
				} `json:"resume"`
			}
			err := json.Unmarshal(a.After.App, &state)
			check(err == nil && state.Resume != nil && state.Resume.MoveOnly == "outcome_unknown", "stale takeover did not report unknown move-only data")
		}
	}
	return failures
}
