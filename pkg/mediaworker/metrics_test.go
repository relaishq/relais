package mediaworker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/srtp/v3"
	"github.com/prometheus/common/expfmt"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func metricsBody(w *Worker) string {
	r := httptest.NewRecorder()
	w.PrivateHandler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return r.Body.String()
}

func checkpointWritesFromScrape(t *testing.T, w *Worker, result string) float64 {
	t.Helper()
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(metricsBody(w)))
	require.NoError(t, err)
	family := families["relais_checkpoint_writes_total"]
	require.NotNil(t, family)
	for _, metric := range family.Metric {
		if len(metric.Label) == 1 && metric.Label[0].GetName() == "result" && metric.Label[0].GetValue() == result {
			return metric.Counter.GetValue()
		}
	}
	t.Fatalf("missing checkpoint write result %q", result)
	return 0
}

func TestWorkerMetricsRetainDecryptionFailuresAfterSessionEnds(t *testing.T) {
	worker := newTestWorker(t)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess := sessionFromState(worker, sessionState{ID: "bad-packets", SRTP: srtpState{Inbound: make(map[uint32]uint64)}})
	sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	sess.handleRTP([]byte{0x80, 96, 0, 1})
	sess.handleRTCP([]byte{0x80, 200, 0, 1})
	require.EqualValues(t, 2, sess.decryptFailures.Load())
	require.Contains(t, metricsBody(worker), "relais_worker_decryption_failures_total 2\n")
	sess.close()
	require.Contains(t, metricsBody(worker), "relais_worker_decryption_failures_total 2\n")
}

func TestWorkerMetricsDoNotCountIdempotentResumeTwice(t *testing.T) {
	source, target := newTestWorker(t), newTestWorker(t)
	call, _ := dialDTLSCaller(t, source)
	state, err := source.ExportSession(call.id)
	require.NoError(t, err)
	lease := sessionstore.Lease{SessionID: call.id, Worker: target.LocalAddr(), Epoch: 1}
	for range 2 {
		id, err := target.ResumeSession(state, ResumeOptions{Lease: lease})
		require.NoError(t, err)
		require.Equal(t, call.id, id)
	}
	require.Contains(t, metricsBody(target), "relais_worker_handovers_resumed_total 1\n")
	require.Contains(t, metricsBody(target), "relais_worker_takeovers_resumed_total 0\n")
	require.NoError(t, target.EndSession(call.id))
	require.Contains(t, metricsBody(target), "relais_worker_active_sessions 0\n")
	require.Contains(t, metricsBody(target), "relais_worker_handovers_resumed_total 1\n")
	_, err = target.ResumeSession([]byte("bad state"), ResumeOptions{SequenceMargin: 8192})
	require.Error(t, err)
	require.Contains(t, metricsBody(target), "relais_worker_takeover_resume_errors_total 1\n")
}
