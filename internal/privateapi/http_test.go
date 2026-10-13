package privateapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestErrorEnvelopePreservesResultAndCauses(t *testing.T) {
	sentinel := errors.New("injected rollback")
	for _, withOutput := range []bool{true, false} {
		t.Run(strconv.FormatBool(withOutput), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				WriteErrorCodes(w, errors.Join(sentinel, Uncertain(context.Canceled)), map[string]error{"rollback": sentinel})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"move failed","result":{"lost":true,"loss_cause":"ownership_expired","error":"rollback failed"}}`))
			}))
			defer server.Close()
			var result struct {
				Lost      bool   `json:"lost"`
				LossCause string `json:"loss_cause"`
				Error     string `json:"error"`
			}
			var output any
			if withOutput {
				output = &result
			}
			err := Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/move", nil, output, map[string]error{"rollback": sentinel})
			require.EqualError(t, err, "move failed")
			require.ErrorIs(t, err, sentinel)
			require.ErrorIs(t, err, ErrUncertain)
			require.ErrorIs(t, err, context.Canceled)
			var failure *HTTPError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, http.StatusConflict, failure.StatusCode)
			if withOutput {
				require.True(t, result.Lost)
				require.Equal(t, "ownership_expired", result.LossCause)
				require.Equal(t, "rollback failed", result.Error)
			}
		})
	}
}

func TestMalformedErrorEnvelopeResultPreservesStatusAndCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteErrorCodes(w, context.DeadlineExceeded, nil)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"move failed","result":"invalid result"}`))
	}))
	defer server.Close()
	var result map[string]any
	err := Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/move", nil, &result, nil)
	require.ErrorContains(t, err, "move failed")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrUncertain)
	var failure *HTTPError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusInternalServerError, failure.StatusCode)
}
