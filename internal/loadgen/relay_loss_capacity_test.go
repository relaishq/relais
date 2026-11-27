package loadgen

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type capacityRoundTrip func(*http.Request) (*http.Response, error)

func (f capacityRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRelayLossCapacityRetryBounds(t *testing.T) {
	for _, mode := range []string{"settled", "full", "cancel", "deadline", "transport", "other-code", "malformed", "missing-retry-after", "get", "restart"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "deadline" {
					var done context.CancelFunc
					ctx, done = context.WithTimeout(ctx, 500*time.Millisecond)
					defer done()
				}
				attempts := 0
				original := &http.Client{Timeout: 10 * time.Second, Transport: capacityRoundTrip(func(r *http.Request) (*http.Response, error) {
					attempts++
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Equal(t, "identical offer", string(body))
					require.Equal(t, "w2", r.URL.Query().Get("worker"))
					require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
					require.Equal(t, "true", r.Header.Get("Relais-Relay-Change"))
					if mode == "transport" {
						return nil, errors.New("acceptance unknown")
					}
					if mode == "cancel" {
						cancel()
					}
					status, bodyText := 503, `{"code":"at_capacity"}`
					if mode == "settled" && attempts == 2 {
						status, bodyText = 201, "answer"
					}
					if mode == "other-code" {
						bodyText = `{"code":"control_not_ready"}`
					}
					if mode == "malformed" {
						bodyText = "not json"
					}
					header := http.Header{"Retry-After": {"1"}}
					if mode == "missing-retry-after" {
						header.Del("Retry-After")
					}
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(bodyText))}, nil
				})}
				client := relayLossHTTPClient(original)
				require.NotSame(t, client, original)
				method := http.MethodPost
				if mode == "get" {
					method = http.MethodGet
				}
				req, err := http.NewRequestWithContext(ctx, method, "http://signal.test/calls?worker=w2", strings.NewReader("identical offer"))
				require.NoError(t, err)
				if mode == "restart" {
					req.URL.Path = "/calls/session/ice-restart"
				}
				req.Header.Set("Content-Type", "application/sdp")
				req.Header.Set("Authorization", "Bearer test-token")
				req.Header.Set("Relais-Relay-Change", "true")
				start := time.Now()
				response, err := client.Do(req)
				if mode == "cancel" || mode == "deadline" || mode == "transport" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					if mode == "settled" {
						require.Equal(t, 201, response.StatusCode)
						require.Equal(t, "answer", string(body))
					} else {
						require.Equal(t, 503, response.StatusCode)
					}
				}
				want := 1
				if mode == "full" {
					want = relayLossCapacityAttempts
				}
				if mode == "settled" {
					want = 2
				}
				require.Equal(t, want, attempts)
				waited := time.Duration(want-1) * time.Second
				if mode == "deadline" {
					waited = 500 * time.Millisecond
				}
				require.Equal(t, waited, time.Since(start))
			})
		})
	}
}
