package loadgen

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const relayLossCapacityAttempts = 4

// Only R opts in. Admission may count a published lease alongside its create
// reservation until that create returns. A/B/M keep their original client.
func relayLossHTTPClient(original *http.Client) *http.Client {
	if original == nil {
		original = &http.Client{Timeout: 10 * time.Second}
	}
	client := *original
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = relayLossCapacityTransport{base}
	return &client
}

type relayLossCapacityTransport struct{ http.RoundTripper }

func (t relayLossCapacityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		response, err := t.RoundTripper.RoundTrip(req)
		if err != nil || attempt == relayLossCapacityAttempts || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/calls") || req.Header.Get("Content-Type") != "application/sdp" || req.GetBody == nil || response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
			return response, err
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		var refusal struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &refusal) != nil || refusal.Code != "at_capacity" {
			return response, nil
		}
		// The existing 20 s dial context and client timeout bound the whole
		// exchange. Only the documented definite refusal can replay an offer.
		timer := time.NewTimer(time.Second)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
		next := req.Clone(req.Context())
		next.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
		req = next
	}
}

func (t relayLossCapacityTransport) CloseIdleConnections() {
	if closer, ok := t.RoundTripper.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
