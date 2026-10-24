package privateapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
)

type statusStore struct {
	*sessionstore.Memory
	unavailable bool
}

func (s *statusStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.unavailable {
		return sessionstore.Lease{}, controlplane.ErrNotReady
	}
	return s.Memory.Get(ctx, id)
}

type statusWorker struct {
	controlplane.Worker // Unused operations deliberately have no implementation.
	store               *sessionstore.Memory
	address             netip.AddrPort
}

func (w statusWorker) CreateSession(ctx context.Context, _ string) (string, string, error) {
	_, err := w.store.Claim(ctx, "fixture-call", w.address, time.Minute)
	return "fixture-call", "answer", err
}

type statusResponse struct {
	Status int            `json:"status"`
	Body   map[string]any `json:"body"`
}

// Exercise the actual anonymous /status error body, rather than a copied type.
func controlUnavailableResponse(t *testing.T) statusResponse {
	t.Helper()
	memory := sessionstore.NewMemory()
	store := &statusStore{Memory: memory}
	p := controlplane.New(nil, store)
	worker := statusWorker{store: memory, address: netip.MustParseAddrPort("192.0.2.1:1234")}
	if err := p.Register("fixture-worker", worker.address, worker); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Create(context.Background(), "offer", "fixture-worker"); err != nil {
		t.Fatal(err)
	}
	store.unavailable = true
	response := httptest.NewRecorder()
	p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /status 503: %d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Pool observation time and elapsed age vary without changing the contract.
	pool, ok := body["pool"].(map[string]any)
	if !ok {
		t.Fatal("status 503 omitted pool")
	}
	pool["sampled_at"] = "2026-10-23T00:00:00Z"
	pool["age_seconds"] = 0
	return statusResponse{Status: response.Code, Body: body}
}
