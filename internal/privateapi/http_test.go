package privateapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponseBoundsAndMalformedErrors(t *testing.T) {
	for _, test := range []struct {
		body    string
		code    int
		message string
	}{
		{"bad json", 200, "invalid character"},
		{strings.Repeat("x", MaxBody+1), 200, "response too large"},
		{"proxy failure", 502, "502 Bad Gateway"},
		{`{"code":"unknown","message":"worker failed"}`, 409, "worker failed"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(test.code)
			_, _ = w.Write([]byte(test.body))
		}))
		var reply map[string]any
		err := Do(context.Background(), server.Client(), server.URL, http.MethodGet, "/", nil, &reply, nil)
		require.ErrorContains(t, err, test.message)
		server.Close()
	}
}

type failedTransport struct{ cause error }

func (f failedTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.cause }
func TestNoResponseIsUncertainAndPreservesCause(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, errors.New("connection reset")} {
		err := Do(context.Background(), &http.Client{Transport: failedTransport{cause}}, "http://127.0.0.1:1", http.MethodPost, "/resume", nil, nil, nil)
		require.ErrorIs(t, err, ErrUncertain)
		require.ErrorIs(t, err, cause)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { Error(w, context.DeadlineExceeded, nil) }))
	defer server.Close()
	err := Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/resume", nil, nil, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrUncertain, "an explicit handler failure has a reply")
}
