package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func resultsRequest(d *demo, body, token, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/demo/results", strings.NewReader(body))
	req.Header.Set("X-Relais-Demo-Token", token)
	req.Header.Set("Content-Type", contentType)
	out := httptest.NewRecorder()
	d.handler(http.NotFoundHandler()).ServeHTTP(out, req)
	return out
}
func TestResultsUploadCreateOnlyAndToken(t *testing.T) {
	d := &demo{control: "http://localhost:12345", launchID: "launch", launchToken: "token"}
	require.NoError(t, d.prepareResults(t.TempDir()))
	existing := filepath.Join(d.resultsDir, "results-launch-1.json")
	require.NoError(t, os.WriteFile(existing, []byte("previous"), 0600))
	require.Equal(t, http.StatusForbidden, resultsRequest(d, `{"version":5}`, "", "application/json").Code)
	require.Equal(t, http.StatusForbidden, resultsRequest(d, `{"version":5}`, "wrong", "application/json").Code)
	out := resultsRequest(d, `{"version":5}`, "token", "application/json")
	require.Equal(t, http.StatusCreated, out.Code)
	require.JSONEq(t, `{"file":"results-launch-2.json"}`, out.Body.String())
	data, err := os.ReadFile(existing)
	require.NoError(t, err)
	require.Equal(t, "previous", string(data))
	data, err = os.ReadFile(filepath.Join(d.resultsDir, "results-launch-2.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{"version":5}`, string(data))
	info, err := os.Stat(filepath.Join(d.resultsDir, "results-launch-2.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	// Concurrent uploads never reuse a sequence or overwrite an earlier record.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			out := resultsRequest(d, `{"version":5}`, "token", "application/json")
			if out.Code != http.StatusCreated {
				t.Errorf("upload: %d", out.Code)
			}
		})
	}
	wg.Wait()
	files, err := filepath.Glob(filepath.Join(d.resultsDir, "results-launch-*.json"))
	require.NoError(t, err)
	require.Len(t, files, 10)
}
func TestResultsUploadBoundsAndDisabled(t *testing.T) {
	d := &demo{control: "http://localhost:12345", launchID: "launch", launchToken: "token"}
	require.Equal(t, http.StatusNotFound, resultsRequest(d, `{}`, "", "application/json").Code)
	d.attached = true
	require.Equal(t, http.StatusNotFound, resultsRequest(d, `{}`, "", "application/json").Code)
	d.attached = false
	require.NoError(t, d.prepareResults(t.TempDir()))
	for _, body := range []string{"", `null`, `[]`, `true`, `{} {}`, `{"broken":`} {
		require.Equal(t, http.StatusBadRequest, resultsRequest(d, body, "token", "application/json").Code, body)
	}
	require.Equal(t, http.StatusUnsupportedMediaType, resultsRequest(d, `{}`, "token", "text/plain").Code)
	exactly := `{"data":"` + strings.Repeat("x", maxResultsBody-len(`{"data":""}`)) + `"}`
	require.Len(t, exactly, maxResultsBody)
	require.Equal(t, http.StatusCreated, resultsRequest(d, exactly, "token", "application/json").Code)
	require.Equal(t, http.StatusRequestEntityTooLarge, resultsRequest(d, exactly+" ", "token", "application/json").Code)
	files, err := os.ReadDir(d.resultsDir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	// A pre-existing symlink cannot redirect a create-only write.
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("protected"), 0600))
	require.NoError(t, os.Symlink(target, filepath.Join(d.resultsDir, fmt.Sprintf("results-launch-%d.json", d.resultsNext+1))))
	require.Equal(t, http.StatusCreated, resultsRequest(d, `{}`, "token", "application/json").Code)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "protected", string(data))
}
