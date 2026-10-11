package privateapi

import (
	"net/http"
	"net/http/httptrace"
	"sync"

	"github.com/relais/internal/movediag"
)

// Allocate trace state only for an enabled, traced coordination request.
type requestDiagnostic struct {
	mu    sync.Mutex
	event movediag.Event
}

func traceRequest(req *http.Request, path string, size int) (*http.Request, *requestDiagnostic) {
	d := &requestDiagnostic{event: movediag.NewEvent(req.Context(), "http", req.Method+" "+DiagnosticOperation(path))}
	d.event.RequestBytes = size
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.event.Connections++
		if info.Reused {
			d.event.ReusedConnections++
		}
	}}))
	movediag.Inject(req)
	return req, d
}
func (d *requestDiagnostic) responseBytes(size int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.event.ResponseBytes = size
}
func (d *requestDiagnostic) finish(failed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	movediag.Default.Finish(d.event, failed)
}
