package callharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/relais/pkg/controlplane"
)

// Status reads the control plane's public HTTP view of ownership and moves.
func (h *Harness) Status(ctx context.Context) (controlplane.Status, error) {
	var status controlplane.Status
	err := h.controlRequest(ctx, http.MethodGet, "/status", &status)
	return status, err
}

// Drain moves all calls off worker through the HTTP endpoint and records
// each move for the caller's gap and consent reports.
func (h *Harness) Drain(worker int) error {
	if worker < 0 || worker >= len(h.workers.list) {
		return fmt.Errorf("callharness: no worker %d", worker)
	}
	var reply controlplane.DrainResult
	if err := h.controlRequest(context.Background(), http.MethodPost, "/workers/"+strconv.Itoa(worker)+"/drain", &reply); err != nil {
		return err
	}

	h.callsMu.Lock()
	defer h.callsMu.Unlock()

	for _, res := range reply.Moves {
		if res.Start.IsZero() {
			continue
		}

		for c := range h.calls {
			if c.SessionID() != res.ID {
				continue
			}

			from, _ := strconv.Atoi(res.From)
			to, _ := strconv.Atoi(res.To)
			var moveErr error
			if res.Error != "" {
				moveErr = errors.New(res.Error)
			}
			c.rec.move(moveRecord{from: from, to: to, start: res.Start, end: res.End, result: res.Result, err: moveErr})
		}
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}

	return nil
}

func (h *Harness) controlRequest(ctx context.Context, method, path string, result any) error {
	if h.workers.relay == nil {
		return errors.New("callharness: control plane needs Options.Relay")
	}

	endpoint := strings.TrimSuffix(h.signalingURL, "/calls") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("callharness: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}
