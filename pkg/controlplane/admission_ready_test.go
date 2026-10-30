package controlplane

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type failingReadyWriter struct{ *httptest.ResponseRecorder }

func (w *failingReadyWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(`"kind":"ready"`)) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(b)
}
func TestReadyFailureKeepsRegisteredInstance(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "register", true: "replace"}[replace], func(t *testing.T) {
			r := newAdmissionRig(t, nil)
			authorizeLongControl(t, r.p)
			accepted, refused := "A", "B"
			cycle := uint64(0)
			if replace {
				stream, err := r.open(t, "A", 1, 0, r.addr)
				require.NoError(t, err)
				stream.run(statusHandler())
				r.declareDead()
				accepted, refused = "B", "A"
				cycle = 1
			}
			var body bytes.Buffer
			require.NoError(t, workerlink.WriteValue(&body, "hello", LinkHello{Instance: accepted, Sequence: 1, Contract: workerlink.ContractVersion, Status: mediaworker.WorkerStatus{InventoryKnown: true}}))
			require.NoError(t, workerlink.WriteValue(&body, "address", LinkAddress{Address: r.addr}))
			req := httptest.NewRequest(http.MethodPost, "/private/workers/a/link", &body)
			req.ProtoMajor = 2
			req.SetPathValue("name", "a")
			r.links.serve(&failingReadyWriter{httptest.NewRecorder()}, req)
			_, err := r.open(t, accepted, 2, cycle+1, r.addr)
			require.Error(t, err, "lost ready must not reopen the restart death-cycle allowance")
			_, err = r.open(t, refused, 2, cycle, r.addr)
			require.Error(t, err, "ready write failure must not admit a different live instance")
			stream, err := r.open(t, accepted, 2, cycle, r.addr)
			require.NoError(t, err, "registered instance must reconnect after a lost ready")
			stream.run(statusHandler())
		})
	}
}
