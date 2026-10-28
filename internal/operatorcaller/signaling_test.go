package operatorcaller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type signalTransport func(*http.Request) (*http.Response, error)

func (f signalTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSignalRTTsPreserveFailureReason(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{"success", 200, nil, ""},
		{"authentication", 401, nil, "caller-signaling-http-401"},
		{"redirect", 302, nil, "caller-signaling-http-302"},
		{"tls", 0, errors.New("certificate verification failed"), "certificate verification failed"},
		{"backend", 502, nil, "caller-signaling-http-502"},
	} {
		t.Run(row.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: signalTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://signal.test/clock" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Fatal("signal request identity differs")
				}
				if row.err != nil {
					return nil, row.err
				}
				return &http.Response{StatusCode: row.status, Header: http.Header{"Location": {"https://other.test/clock"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			rtts, err := SignalRTTs(context.Background(), "https://signal.test/calls", "fixture-token", client)
			if row.want == "" {
				if err != nil || len(rtts) != ClockBurstCount || calls != ClockBurstCount {
					t.Fatal("successful probe burst differs", rtts, err, calls)
				}
			} else if err == nil || !strings.Contains(err.Error(), row.want) || calls != 1 || len(rtts) != 0 {
				t.Fatal("signal failure lost its reason or was retried", err, calls)
			}
		})
	}
}
