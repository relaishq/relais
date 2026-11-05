package loadgen

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestServeClockProbesAfterCursor(t *testing.T) {
	at := time.Date(2026, 11, 5, 1, 0, 0, 123456789, time.UTC)
	probes := make([]callharness.ClockProbe, 5)
	for i := range probes {
		send := at.Add(time.Duration(i) * time.Second)
		probes[i] = callharness.ClockProbe{CallerSend: send, CallerReceive: send.Add(time.Millisecond), ServerReceive: send, ServerSend: send}
	}
	get := func(target string) (int, []callharness.ClockProbe) {
		reply := httptest.NewRecorder()
		ServeClockProbes(reply, httptest.NewRequest(http.MethodGet, target, nil), probes)
		var out []callharness.ClockProbe
		if reply.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &out))
		}
		return reply.Code, out
	}
	// Ready evidence still reads the whole list without a cursor.
	code, all := get("/clock-probes")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, len(probes), len(all))
	for _, after := range []int{0, 2, 5} {
		code, got := get("/clock-probes?after=" + strconv.Itoa(after))
		require.Equal(t, http.StatusOK, code)
		require.Len(t, got, len(probes)-after, "MUTATION: probe-cursor-ignored")
		if after < len(probes) {
			require.True(t, got[0].CallerSend.Equal(probes[after].CallerSend))
		}
	}
	for _, bad := range []string{"6", "-1", "x", ""} {
		code, _ := get("/clock-probes?after=" + bad)
		require.Equal(t, http.StatusBadRequest, code, "MUTATION: probe-cursor-unchecked %q", bad)
	}
}
