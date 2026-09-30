package metrics

import (
	"net/http"
	"strconv"
	"time"
)

// statusRecorder wraps ResponseWriter to capture the status code.
type statusRecorder struct {
	w      http.ResponseWriter
	status int
}

func (r *statusRecorder) Header() http.Header         { return r.w.Header() }
func (r *statusRecorder) Write(b []byte) (int, error) { return r.w.Write(b) }
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.w.WriteHeader(code)
}

// Middleware provides basic HTTP server metrics: in-flight, duration, totals, and panic recovery.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path // consider normalizing if cardinality becomes high
		method := r.Method

		HTTPInFlight.Inc()
		defer HTTPInFlight.Dec()

		rec := &statusRecorder{w: w, status: http.StatusOK}
		start := time.Now()

		served := false
		defer func() {
			if panicVal := recover(); panicVal != nil {
				HTTPPanicsTotal.Inc()
				// best-effort 500 if handler panicked before writing
				if !served {
					rec.WriteHeader(http.StatusInternalServerError)
					_, _ = rec.Write([]byte(http.StatusText(http.StatusInternalServerError)))
				}
			}
			dur := time.Since(start).Seconds()
			codeStr := strconv.Itoa(rec.status)
			HTTPRequestDuration.WithLabelValues(method, path, codeStr).Observe(dur)
			HTTPRequestsTotal.WithLabelValues(method, path, codeStr).Inc()
		}()

		next.ServeHTTP(rec, r)
		served = true
	})
}
