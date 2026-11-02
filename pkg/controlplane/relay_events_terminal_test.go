package controlplane

import (
	"bufio"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayEventsEndOnTerminalLoss(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, b, id := relayFailurePlane(t, store)
		p.workers["b"].worker = &takeoverWorker{fakeWorker: b}
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		response, err := server.Client().Get(server.URL + "/calls/" + id + "/events")
		require.NoError(t, err)
		defer response.Body.Close()
		ended := make(chan bool, 1)
		go func() {
			scan := bufio.NewScanner(response.Body)
			for scan.Scan() {
				if scan.Text() == "event: ended" {
					ended <- true
					return
				}
			}
			ended <- false
		}()
		lease, err := store.Get(context.Background(), id)
		require.NoError(t, err)
		source := p.workers["a"]
		p.mu.Lock()
		expireObservedWorker(t, source)
		p.mu.Unlock()
		p.recovery.recover(context.Background(), source, lease, time.Now()) // no checkpoint: terminal loss
		select {
		case ok := <-ended:
			require.True(t, ok)
		case <-time.After(time.Second):
			t.Fatal("terminal loss left event stream open")
		}
	})
}
