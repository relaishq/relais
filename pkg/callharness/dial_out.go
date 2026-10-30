package callharness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/common/expfmt"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
)

// Dial-out harness workers use the real HTTP/2 command link and UDP hello.
// Inbound workers retain the local interface used by existing harness tests.
func (t *relayTopology) startLinks(ctx context.Context, indices []int, count int) error {
	names := make([]string, 0, len(indices))
	seen := map[int]bool{}
	for _, i := range indices {
		if i < 0 || i >= count || seen[i] {
			return fmt.Errorf("callharness: invalid dial-out worker index %d", i)
		}
		seen[i] = true
		names = append(names, strconv.Itoa(i))
	}
	relayAPI := httptest.NewServer(t.relay.PrivateHandler())
	t.links = t.plane.ConfigureDialOut(names, &controlplane.RemoteRelay{URL: relayAPI.URL})
	var blocked sync.Map
	t.linkBlocked = &blocked
	handler := t.links.Handler(t.plane.ProcessHandler())
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "" {
			if _, ok := blocked.Load(r.URL.Path); ok {
				http.Error(w, "link unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Start()
	t.linkURL = server.URL
	t.linkDone = make(chan struct{})
	go func() {
		<-ctx.Done()
		t.links.Close()
		t.linkWorkers.Wait()
		server.Close()
		relayAPI.Close()
		close(t.linkDone)
	}()
	return nil
}
func (t *relayTopology) startDialOut(ctx context.Context, index int, worker *mediaworker.Worker) error {
	link := &controlplane.DialOut{URL: t.linkURL, Name: strconv.Itoa(index), Worker: worker, Ready: make(chan struct{})}
	t.linkWorkers.Go(func() { _ = link.Run(ctx) })
	t.linkWorkers.Go(func() { worker.KeepRelayBinding(ctx) })
	select {
	case <-link.Ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return fmt.Errorf("callharness: dial-out registration timed out")
	}
}

// SetWorkerLink blocks/re-enables only one worker's command link. HTTP
// heartbeats continue independently. It provides reset faults without polling.
func (h *Harness) SetWorkerLink(index int, up bool) error {
	t := h.workers.relay
	if t == nil || t.links == nil {
		return fmt.Errorf("callharness: no command links")
	}
	name := strconv.Itoa(index)
	path := "/private/workers/" + name + "/link"
	if up {
		t.linkBlocked.Delete(path)
	} else {
		t.linkBlocked.Store(path, true)
		t.links.Drop(name)
	}
	return nil
}

// WorkerDecryptionFailures sums the in-process workers' lifetime counters,
// including ended sessions and hard-killed workers retained by the harness.
func (h *Harness) WorkerDecryptionFailures() (float64, error) {
	if h.external != nil {
		return 0, fmt.Errorf("callharness: worker counters require in-process workers")
	}
	var total float64
	for _, worker := range h.workers.list {
		reply := httptest.NewRecorder()
		worker.PrivateHandler().ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		var parser expfmt.TextParser
		families, err := parser.TextToMetricFamilies(reply.Body)
		if err != nil {
			return 0, err
		}
		family := families["relais_worker_decryption_failures_total"]
		if family == nil || len(family.Metric) != 1 {
			return 0, fmt.Errorf("callharness: worker decryption counter unavailable")
		}
		total += family.Metric[0].GetCounter().GetValue()
	}
	return total, nil
}
