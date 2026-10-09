package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/relais/internal/relayleg"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// A real relay timer releases the hold while resume is blocked. The HTTP
// caller must see a successful move/drain with a warning, not a retryable error.
func TestSuccessfulMoveAfterHoldTimeout(t *testing.T) {
	for _, action := range []string{"move", "drain"} {
		t.Run(action, func(t *testing.T) {
			store := sessionstore.NewMemory()
			r, err := relay.New(relay.Config{Owners: store, HoldTimeout: 100 * time.Millisecond})
			require.NoError(t, err)
			t.Cleanup(func() { _ = r.Close() })
			listen := func() *net.UDPConn {
				conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				return conn
			}
			source, target := listen(), listen()
			a := &fakeWorker{store: store, addr: source.LocalAddr().(*net.UDPAddr).AddrPort(), running: map[string]bool{}}
			b := &fakeWorker{store: store, addr: target.LocalAddr().(*net.UDPAddr).AddrPort(), running: map[string]bool{}, resumeStarted: make(chan struct{}), resumeGate: make(chan struct{})}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(b.resumeGate) }) })
			p := New(r, store)
			for name, worker := range map[string]*fakeWorker{"a": a, "b": b} {
				r.AddWorker(worker.addr)
				require.NoError(t, p.Register(name, worker.addr, worker))
			}
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)

			acked := make(chan error, 1)
			go func() {
				_ = source.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 64)
				n, _, err := source.ReadFromUDPAddrPort(buf)
				if err == nil {
					nonce, _, valid := relayleg.ParseBarrier(buf[:n])
					if !valid {
						acked <- relay.ErrMalformedHeader
						return
					}
					_, err = source.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), r.WorkerAddr())
				}
				acked <- err
			}()
			path := "/calls/" + id + "/move?to=b"
			if action == "drain" {
				path = "/workers/a/drain"
			}
			response := httptest.NewRecorder()
			finished := make(chan struct{})
			go func() {
				p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
				close(finished)
			}()
			select {
			case <-b.resumeStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("target did not start resume")
			}
			require.NoError(t, <-acked)
			require.Eventually(t, func() bool { return r.Stats().HoldTimeouts == 1 }, time.Second, time.Millisecond)
			release.Do(func() { close(b.resumeGate) })
			<-finished
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result MoveResult
			if action == "drain" {
				var drain DrainResult
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &drain))
				require.Empty(t, drain.Error)
				require.Len(t, drain.Moves, 1)
				result = drain.Moves[0]
			} else {
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			}
			require.Empty(t, result.Error)
			raw, err := json.Marshal(result.Result)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(raw, &fields))
			require.Equal(t, true, fields["HoldExpired"], "expiry warning is visible in the move report")
			require.True(t, b.runs(id))
			require.False(t, a.runs(id))
			status, err := p.Status(context.Background())
			require.NoError(t, err)
			require.Len(t, status.Calls, 1)
			require.Equal(t, "b", status.Calls[0].Owner)
			require.EqualValues(t, 1, status.Calls[0].MoveCount)
			require.EqualValues(t, 1, r.Stats().HoldTimeouts, "one counted warning, one completed move")
		})
	}
}
