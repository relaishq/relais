package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func relayStatusOf(t *testing.T, p *Plane, id string) *CallRelayStatus {
	t.Helper()
	status, err := p.Status(context.Background())
	require.NoError(t, err)
	for _, c := range status.Calls {
		if c.ID == id {
			return c.Relay
		}
	}
	t.Fatalf("call %s missing from status", id)
	return nil
}

// ADR 0016 section 7: private status names the assignment, the accepted mode
// and the current attempt's delivered and decoded MID sets. Each generation
// starts undelivered; evidence never carries over to a higher generation.
func TestStatusShowsRelayConfirmation(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, &CallRelayStatus{Slot: "r1", Confirmation: sessionstore.RelayConfirmDecode}, relayStatusOf(t, p, id))

		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id)
		// C arrives at once; A never does, so this generation stays unconfirmed.
		require.Eventually(t, func() bool {
			r := relayStatusOf(t, p, id)
			return r.Attempt != nil && r.Attempt.Delivered
		}, 2*time.Second, 10*time.Millisecond)
		require.Equal(t, &CallRelayStatus{Slot: "r2", Generation: 1, Confirmation: sessionstore.RelayConfirmDecode, Attempt: &RelayAttemptStatus{
			Generation: 1, Slot: "r2", Phase: sessionstore.RelayPhaseSwitched, Kind: "planned",
			RequiredMIDs: []string{"0"}, Delivered: true, DeliveredMIDs: []string{"0"}, DecodedMIDs: []string{},
		}}, relayStatusOf(t, p, id), "complete C without A is delivery, not confirmation")

		// The attempt bound rolls back to r1 with a fresh, undelivered generation.
		lease, rollback := waitRelayPendingAt(t, store, id, 1)
		require.Eventually(t, func() bool {
			r := relayStatusOf(t, p, id)
			return r.Attempt != nil && r.Attempt.Generation == 2
		}, 2*time.Second, 10*time.Millisecond)
		got := relayStatusOf(t, p, id)
		require.Equal(t, "r1", got.Pending)
		require.Equal(t, &RelayAttemptStatus{Generation: 2, Slot: "r1", Phase: sessionstore.RelayPhasePending, Kind: "planned",
			RequiredMIDs: []string{"0"}, DeliveredMIDs: []string{}, DecodedMIDs: []string{}}, got.Attempt)

		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, rollback, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}))
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":2,"decoded_mids":["0"]}`).StatusCode)
		require.ErrorIs(t, <-done, ErrRelayRolledBack)
		got = relayStatusOf(t, p, id)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, got.Attempt.Phase)
		require.Equal(t, []string{"0"}, got.Attempt.DecodedMIDs)

		// The HTTP view carries the same fields.
		resp, err := http.Get(server.URL + "/status")
		require.NoError(t, err)
		defer resp.Body.Close()
		var raw struct {
			Calls []struct {
				Relay map[string]any `json:"relay"`
			} `json:"calls"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))
		require.Len(t, raw.Calls, 1)
		require.Equal(t, "decode", raw.Calls[0].Relay["confirmation"])
		require.Equal(t, "confirmed", raw.Calls[0].Relay["attempt"].(map[string]any)["phase"])
	})
}

// Calls on a plane without a relay set have no relay status and cost no
// extra store read.
func TestStatusOmitsRelayWithoutRelaySet(t *testing.T) {
	store := sessionstore.NewMemory()
	p := New(&fakeRelay{}, store)
	w := &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17001"), running: map[string]bool{}}
	require.NoError(t, p.Register("a", w.addr, w))
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	require.Nil(t, relayStatusOf(t, p, id))
}

// A call without a saved mode on a relay set reads as mode none (ADR 0016
// section 10 rule 3).
func TestStatusShowsOrdinaryCallOnRelaySet(t *testing.T) {
	p, _, _, id := relayFailurePlane(t, sessionstore.NewMemory(), "")
	require.Equal(t, &CallRelayStatus{Slot: "r1", Confirmation: sessionstore.RelayConfirmNone}, relayStatusOf(t, p, id))
}

// The status phase follows the single completion predicate, so a commit that
// supplies the last prerequisite shows as confirmed, and a retired generation
// as aborted, even before control rewrites the stored phase.
func TestRelayAttemptStatusPhase(t *testing.T) {
	attempt := func(phase string, decoded, delivered []string) *sessionstore.RelayAttempt {
		return &sessionstore.RelayAttempt{Generation: 1, Slot: "r2", Phase: phase, Kind: "planned", Required: []string{"0", "1"}, Decoded: decoded, Delivered: delivered}
	}
	switched := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
	pending := sessionstore.RelayAssignment{Slot: "r1", Pending: "r2"}
	for _, tc := range []struct {
		name, want string
		rec        sessionstore.RelayConfirmation
		a          sessionstore.RelayAssignment
		delivered  bool
	}{
		{"pending", sessionstore.RelayPhasePending, sessionstore.RelayConfirmation{Mode: "decode", Attempt: attempt("pending", nil, nil)}, pending, false},
		{"commit observed later", sessionstore.RelayPhaseSwitched, sessionstore.RelayConfirmation{Mode: "decode", Attempt: attempt("pending", nil, []string{"0"}), Commit: sessionstore.RelayCommit{Switched: 1}}, switched, false},
		{"commit supplies the last prerequisite", sessionstore.RelayPhaseConfirmed, sessionstore.RelayConfirmation{Mode: "decode", Attempt: attempt("pending", []string{"0", "1"}, nil), Commit: sessionstore.RelayCommit{Switched: 1}}, switched, false},
		{"delivery mode, full C", sessionstore.RelayPhaseConfirmed, sessionstore.RelayConfirmation{Mode: "delivery", Attempt: attempt("switched", nil, []string{"1", "0"}), Commit: sessionstore.RelayCommit{Switched: 1}}, switched, true},
		{"decode mode, full C only", sessionstore.RelayPhaseSwitched, sessionstore.RelayConfirmation{Mode: "decode", Attempt: attempt("switched", []string{"0"}, []string{"0", "1"}), Commit: sessionstore.RelayCommit{Switched: 1}}, switched, true},
		{"retired without a switch", sessionstore.RelayPhaseAborted, sessionstore.RelayConfirmation{Mode: "decode", Attempt: attempt("pending", []string{"0", "1"}, nil), Commit: sessionstore.RelayCommit{Retired: 1}}, sessionstore.RelayAssignment{Slot: "r1", Generation: 1}, false},
		{"expired", sessionstore.RelayPhaseExpired, sessionstore.RelayConfirmation{Mode: "delivery", Attempt: attempt("expired", nil, []string{"0", "1"}), Commit: sessionstore.RelayCommit{Switched: 1}}, switched, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := relayAttemptStatus(tc.rec, tc.a)
			require.Equal(t, tc.want, got.Phase)
			require.Equal(t, tc.delivered, got.Delivered)
		})
	}
}
