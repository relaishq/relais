package loadgen

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
)

// The R judge decodes this envelope itself. Message and callharness.Report
// remain unchanged, as do the shared recursive evidence decoders.
type relayLossMessage struct {
	Message
	Episodes []callharness.RelayRestartEpisode `json:"relayLossEpisodes,omitempty"`
}

func (w *Writer) writeRelayLoss(m Message, episodes []callharness.RelayRestartEpisode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = w.encoder.Encode(relayLossMessage{m, episodes})
	}
}

func (o ChildOptions) callOptions(i int, history time.Duration, meter *Meter) callharness.CallOptions {
	worker := -1
	if o.PlaceOnWorker {
		worker = 0
	}
	opts := callharness.CallOptions{Video: o.Video, Worker: worker, Recording: callharness.RecordingOptions{History: history, DecodeEvery: 30, MaintenancePhase: o.Desync.Maintenance(i)}, SendTiming: meter.Observe, SendPacing: meter.ObservePacing}
	if o.RelayLoss {
		opts.RelayChange, opts.RelayLossEvidence = true, true
		opts.ExternalWorker = []string{"w1", "w2"}[i/2]
	}
	return opts
}

// Registration still needs its original near-caller burst. R does not create a
// ClockTracker, distributed events or a background clock-mapping sampler.
func registerRelayLossProbes(mux *http.ServeMux, signaling string, client *http.Client) {
	var once sync.Once
	var probes []callharness.ClockProbe
	var probeErr error
	mux.HandleFunc("GET /clock-probes", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			for range 10 {
				probe, err := callharness.ProbeClock(r.Context(), client, strings.TrimSuffix(signaling, "/calls")+"/clock")
				if err != nil {
					probeErr = err
					return
				}
				probes = append(probes, probe)
			}
		})
		if probeErr != nil {
			http.Error(w, "caller registration probes unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(probes)
	})
}
