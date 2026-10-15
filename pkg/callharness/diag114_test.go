package callharness

// Investigation-only instrumentation for #114. It records the control plane's
// own /status view and the test's kill timestamps as JSON lines, without
// changing product behaviour. Set DIAG114_DIR to collect records.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/relay"
)

type diag114Record struct {
	Event       string               `json:"event"`
	Test        string               `json:"test"`
	PID         int                  `json:"pid"`
	At          time.Time            `json:"at"`
	Failed      bool                 `json:"failed,omitempty"`
	Want        int                  `json:"want,omitempty"`
	WaitStart   time.Time            `json:"wait_start,omitzero"`
	LateAt      time.Time            `json:"late_at,omitzero"` // when the wanted count appeared after the 3 s window
	LateCount   int                  `json:"late_count,omitempty"`
	Marks       map[string]time.Time `json:"marks,omitempty"`
	Status      *controlplane.Status `json:"status,omitempty"`
	StatusError string               `json:"status_error,omitempty"`
	Relay       *relay.Stats         `json:"relay,omitempty"`
}

var diag114Mu sync.Mutex

func diag114Write(t *testing.T, rec diag114Record) {
	rec.Test, rec.PID, rec.At = t.Name(), os.Getpid(), time.Now()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Logf("DIAG114 marshal: %v", err)
		return
	}
	dir := os.Getenv("DIAG114_DIR")
	if dir == "" {
		t.Logf("DIAG114 %s", data)
		return
	}
	diag114Mu.Lock()
	defer diag114Mu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("diag-%d.jsonl", os.Getpid())), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("DIAG114 open: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

type diag114Marks struct {
	mu    sync.Mutex
	marks map[string]time.Time
}

func (m *diag114Marks) mark(name string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.marks == nil {
		m.marks = map[string]time.Time{}
	}
	m.marks[name] = at
}

func (m *diag114Marks) snapshot() map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Time, len(m.marks))
	for k, v := range m.marks {
		out[k] = v
	}
	return out
}

// diag114Late keeps polling after the 3 s window, with a fresh context, to
// show whether a missed takeover is merely late, lost, or never completes.
func diag114Late(h *Harness, n int, limit time.Duration) (time.Time, controlplane.Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit+time.Second)
	defer cancel()
	deadline := time.Now().Add(limit)
	var status controlplane.Status
	var err error
	for time.Now().Before(deadline) {
		status, err = h.Status(ctx)
		if err == nil && len(status.Takeovers) >= n {
			return time.Now(), status, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Time{}, status, err
}
