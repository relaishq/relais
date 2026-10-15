package callharness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestExternalMoveFailedOwnerIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "failed", http.StatusInternalServerError) }))
	defer server.Close()
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:20000")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	c := &Call{harness: h, rec: newRecorder(), remoteUfrag: "test-call"}
	if err := c.MoveExternal(context.Background(), 1); err == nil {
		t.Fatal("move unexpectedly succeeded")
	}
	if len(c.rec.moves) != 1 || c.rec.moves[0].from != -1 || c.rec.moves[0].err == nil {
		t.Fatalf("failed move owner is not unknown: %+v", c.rec.moves)
	}
}

func TestRecordOperatorDrainPreservesCallerMove(t *testing.T) {
	rec := newRecorder()
	started := rec.start.Add(time.Second)
	rec.move(moveRecord{from: 0, to: 1, start: started, end: started.Add(time.Millisecond)})
	// The observer must keep the actual API times and worker IDs.
	c := &Call{rec: rec}
	c.RecordExternalMove(controlplane.MoveResult{From: "1", To: "2", Start: started.Add(time.Second), End: started.Add(time.Second + time.Millisecond)})
	if len(rec.moves) != 2 || rec.moves[1].from != 1 || rec.moves[1].to != 2 || !rec.moves[1].start.Equal(started.Add(time.Second)) {
		t.Fatalf("wrong drain observation: %+v", rec.moves)
	}
}

func TestExternalMoveRecordsFailedResult(t *testing.T) {
	for _, withError := range []bool{true, false} {
		t.Run(strconv.FormatBool(withError), func(t *testing.T) {
			rec := newRecorder()
			result := controlplane.MoveResult{
				ID: "test-call", Kind: "move", From: "0", To: "1", Start: rec.start.Add(time.Second), End: rec.start.Add(2 * time.Second),
				Lost: true, LossCause: "ownership_expired", Error: "server move failed",
				Result: mediaworker.HandoverResult{SessionID: "test-call", MoveDiagnosticID: "diagnostic", RolledBack: true, Export: time.Millisecond},
			}
			if !withError {
				result.Error = ""
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/calls/test-call/move?to=1", r.URL.RequestURI())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				privateapi.Write(w, controlplane.MoveFailure{Error: "transport reply uncertain", Codes: privateapi.ErrorCodes(privateapi.Uncertain(context.DeadlineExceeded), controlplane.RemoteErrors), Result: result})
			}))
			defer server.Close()
			h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:20000")}})
			require.NoError(t, err)
			defer func() { require.NoError(t, h.Close()) }()
			c := &Call{harness: h, rec: rec, remoteUfrag: "test-call"}
			err = c.MoveExternal(context.Background(), 1)
			require.ErrorIs(t, err, privateapi.ErrUncertain)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			var failure *privateapi.HTTPError
			require.True(t, errors.As(err, &failure))
			require.Equal(t, http.StatusInternalServerError, failure.StatusCode)
			reports := rec.moveReports()
			require.Len(t, reports, 1)
			require.Equal(t, 0, reports[0].From)
			require.Equal(t, 1, reports[0].To)
			require.Equal(t, time.Second, reports[0].Start)
			require.Equal(t, 2*time.Second, reports[0].End)
			require.Equal(t, result.Result, reports[0].Result)
			require.True(t, reports[0].Lost)
			require.Equal(t, result.LossCause, reports[0].LossCause)
			if withError {
				require.Equal(t, result.Error, reports[0].Error)
			} else {
				require.Equal(t, err.Error(), reports[0].Error)
			}
		})
	}
}
