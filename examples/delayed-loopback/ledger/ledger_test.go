package ledger

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeliveredAtLeastOnceCommittedOnceAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path)
	require.NoError(t, err)
	commit := func(l *Ledger, value string) int {
		body, err := json.Marshal(struct {
			Key   string `json:"key"`
			Value []byte `json:"value"`
		}{"same-logical-task", []byte(value)})
		require.NoError(t, err)
		response := httptest.NewRecorder()
		l.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(body)))
		return response.Code
	}
	require.Equal(t, 200, commit(l, "effect"))
	require.Equal(t, 200, commit(l, "effect"))
	deliveries, commits := l.Counts()
	require.Equal(t, 2, deliveries)
	require.Equal(t, 1, commits)
	require.Equal(t, 409, commit(l, "different effect"))
	require.NoError(t, l.Close())
	l, err = Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, l.Close()) }()
	require.Equal(t, 200, commit(l, "effect"))
	_, commits = l.Counts()
	require.Equal(t, 1, commits)
}
func TestOnlyLoopback(t *testing.T) {
	for _, url := range []string{"", "https://127.0.0.1:1234", "http://example.com:80", "http://127.0.0.1:0", "http://127.0.0.1:1234/path", "http://u:p@127.0.0.1:1234", "http://127.0.0.1:1234?x=y"} {
		_, err := localURL(url)
		require.Error(t, err)
	}
	_, err := localURL("http://127.0.0.1:1234")
	require.NoError(t, err)
}

func TestRefusesUnterminatedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	data, err := json.Marshal(entry{Version: 2, Key: "one", Value: []byte("effect"), Number: 1})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	l, err := Open(path)
	if err == nil {
		require.NoError(t, l.Close())
	}
	require.Error(t, err, "an interrupted append must not concatenate the next record")
}

func TestReopensLegacyTextAndBinaryRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"version\":1,\"key\":\"old\",\"value\":\"effect\",\"number\":1}\n"), 0600))
	l, err := Open(path)
	require.NoError(t, err)
	body := []byte(`{"key":"new","value":"/w=="}`)
	reply := httptest.NewRecorder()
	l.ServeHTTP(reply, httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, reply.Code)
	require.NoError(t, l.Close())
	l, err = Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, l.Close()) }()
	for _, body := range []string{`{"key":"old","value":"ZWZmZWN0"}`, `{"key":"new","value":"/w=="}`} {
		reply := httptest.NewRecorder()
		l.ServeHTTP(reply, httptest.NewRequest(http.MethodPost, "/commit", bytes.NewBufferString(body)))
		require.Equal(t, http.StatusOK, reply.Code)
	}
	_, commits := l.Counts()
	require.Equal(t, 2, commits)
}
