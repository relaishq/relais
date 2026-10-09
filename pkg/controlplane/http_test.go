package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPOfferMoveStatusAndHangup(t *testing.T) {
	p, _, _, _ := setup(t)
	handler := p.Handler()
	request := func(method, path, body, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		return rw
	}
	invalid := request(http.MethodPost, "/calls", "offer", "text/plain")
	require.Equal(t, http.StatusUnsupportedMediaType, invalid.Code)
	created := request(http.MethodPost, "/calls?worker=a", "offer", "application/sdp")
	require.Equal(t, http.StatusCreated, created.Code)
	require.Equal(t, "answer", created.Body.String())
	resource := created.Header().Get("Location")
	require.NotEmpty(t, resource)
	moved := request(http.MethodPost, resource+"/move", "", "") // automatic target
	require.Equal(t, http.StatusOK, moved.Code, moved.Body.String())
	var move MoveResult
	require.NoError(t, json.Unmarshal(moved.Body.Bytes(), &move))
	require.Equal(t, "a", move.From)
	require.Equal(t, "b", move.To)
	current := request(http.MethodGet, "/status", "", "")
	require.Equal(t, http.StatusOK, current.Code)
	var status Status
	require.NoError(t, json.Unmarshal(current.Body.Bytes(), &status))
	require.Len(t, status.Calls, 1)
	require.Equal(t, "b", status.Calls[0].Owner)
	require.NotNil(t, status.Calls[0].LastMove)
	drained := request(http.MethodPost, "/workers/b/drain", "", "")
	require.Equal(t, http.StatusOK, drained.Code, drained.Body.String())
	hungup := request(http.MethodDelete, resource, "", "")
	require.Equal(t, http.StatusOK, hungup.Code, hungup.Body.String())
	missing := request(http.MethodDelete, resource, "", "")
	require.Equal(t, http.StatusNotFound, missing.Code)
}
