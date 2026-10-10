package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestDemoRegistrationIsOptInAndRegistersFreshWorker(t *testing.T) {
	store := sessionstore.NewMemory()
	relayAdds := 0
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/workers", r.URL.Path)
		relayAdds++
		privateapi.Write(w, map[string]bool{"ok": true})
	}))
	defer relayServer.Close()
	relay := &controlplane.RemoteRelay{URL: relayServer.URL}
	plane := controlplane.New(relay, store)
	normal := httptest.NewServer(plane.ProcessHandler())
	defer normal.Close()
	req, err := http.NewRequest(http.MethodPost, normal.URL+"/demo/register", nil)
	require.NoError(t, err)
	resp, err := normal.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_ = resp.Body.Close()
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/status", r.URL.Path)
		privateapi.Write(w, mediaworker.WorkerStatus{Address: netip.MustParseAddrPort("127.0.0.1:23456")})
	}))
	defer workerServer.Close()
	demo := httptest.NewServer(demoHandler(plane, relay))
	defer demo.Close()
	require.NoError(t, privateapi.Do(context.Background(), demo.Client(), demo.URL, http.MethodPost, "/demo/register", map[string]string{"name": "fresh", "url": workerServer.URL}, nil, nil))
	status, err := plane.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Workers, 1)
	require.Equal(t, "fresh", status.Workers[0].Name)
	require.False(t, status.Workers[0].Draining)
	require.Equal(t, 1, relayAdds)
	for _, address := range []string{"http://example.com:80", "http://127.0.0.1:12345/private", "https://127.0.0.1:12345", "http://user:password@127.0.0.1:12345"} {
		err := privateapi.Do(context.Background(), demo.Client(), demo.URL, http.MethodPost, "/demo/register", map[string]string{"name": "bad", "url": address}, nil, nil)
		require.Error(t, err)
	}
}
