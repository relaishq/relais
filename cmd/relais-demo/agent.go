package main

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/relais/internal/privateapi"
	agentdemo "github.com/relais/pkg/agent/demo"
	"github.com/relais/pkg/controlplane"
)

// Continuity uses independent source/export and target/resume observations.
// Missing reads remain explicit, and cannot become a passing browser verdict.
type agentContinuity struct {
	Before                  *agentdemo.Status   `json:"before"`
	After                   *agentdemo.Status   `json:"after"`
	Exported                *agentdemo.Evidence `json:"exported,omitempty"`
	MeasuredCheckpointAgeMS *float64            `json:"measured_checkpoint_age_ms,omitempty"`
	MeasuredSnapshotAgeMS   *float64            `json:"measured_snapshot_age_ms,omitempty"`
	Errors                  []string            `json:"errors"`
}

func (d *demo) readAgent(ctx context.Context, id, owner string) (*agentdemo.Status, error) {
	d.mu.Lock()
	worker := d.workers[owner]
	d.mu.Unlock()
	if worker == nil {
		return nil, controlplane.ErrUnknownCall
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var status agentdemo.Status
	err := privateapi.Do(ctx, d.client, worker.URL, http.MethodGet, "/demo/agents/"+url.PathEscape(id), nil, &status, nil)
	if err != nil {
		return nil, err
	}
	return &status, nil
}
func (d *demo) beforeAgent(ctx context.Context, id, owner string) *agentContinuity {
	if d.agentName != "demo" {
		return nil
	}
	result := &agentContinuity{Errors: []string{}}
	var err error
	result.Before, err = d.readAgent(ctx, id, owner)
	if err != nil {
		result.Errors = append(result.Errors, "before: "+err.Error())
	}
	return result
}
func (d *demo) finishAgent(ctx context.Context, id, from, to, kind string, result *agentContinuity) {
	if result == nil {
		return
	}
	if kind != "kill" {
		exported, err := d.readAgent(ctx, id, from)
		if err != nil {
			result.Errors = append(result.Errors, "export: "+err.Error())
		} else {
			result.Exported = exported.Exported
		}
	}
	var err error
	result.After, err = d.readAgent(ctx, id, to)
	if err != nil {
		result.Errors = append(result.Errors, "after: "+err.Error())
		return
	}
	if kind == "kill" {
		status, err := d.status(ctx)
		if err != nil {
			result.Errors = append(result.Errors, "checkpoint age: "+err.Error())
			return
		}
		for i := len(status.Takeovers) - 1; i >= 0; i-- {
			event := status.Takeovers[i]
			if event.ID == id && event.From == from && event.To == to {
				age, snapshot := float64(event.CheckpointAge)/1e6, float64(event.SnapshotAge)/1e6
				result.MeasuredCheckpointAgeMS = &age
				result.MeasuredSnapshotAgeMS = &snapshot
				break
			}
		}
	}
}

func (d *demo) moveAgent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	id := r.PathValue("id")
	owner, err := d.owner(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	continuity := d.beforeAgent(ctx, id, owner.Owner)
	var move controlplane.MoveResult
	path := "/calls/" + url.PathEscape(id) + "/move"
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	err = privateapi.Do(ctx, d.client, d.control, http.MethodPost, path, nil, &move, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	d.finishAgent(ctx, id, move.From, move.To, "move", continuity)
	privateapi.Write(w, struct {
		controlplane.MoveResult
		Agent *agentContinuity `json:"agent_continuity"`
	}{move, continuity})
}
