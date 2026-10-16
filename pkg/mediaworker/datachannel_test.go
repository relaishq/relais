package mediaworker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func dataSDPOffer(media bool) string {
	raw := testOffer(testOfferAttrs{})
	raw = strings.Replace(raw, "a=group:BUNDLE 0 1", "a=group:BUNDLE 0 1 data", 1)
	raw += "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:data\r\na=sctp-port:5000\r\na=max-message-size:65536\r\na=setup:actpass\r\n"
	if !media {
		start := strings.Index(raw, "m=audio")
		end := strings.Index(raw, "m=application")
		raw = raw[:start] + raw[end:]
		raw = strings.Replace(raw, "a=group:BUNDLE 0 1 data", "a=group:BUNDLE data", 1)
		// testOffer carries credentials/fingerprint on the audio section.
		original := testOffer(testOfferAttrs{})
		for _, line := range strings.Split(original, "\r\n") {
			if strings.HasPrefix(line, "a=ice-ufrag:") || strings.HasPrefix(line, "a=ice-pwd:") || strings.HasPrefix(line, "a=fingerprint:") {
				raw += line + "\r\n"
			}
		}
	}
	return raw
}
func TestDataChannelOfferOptInAndValidation(t *testing.T) {
	cfg := &DataChannelConfig{Upstream: "ws://127.0.0.1/", MaxMessage: 1024}
	for _, media := range []bool{false, true} {
		raw := dataSDPOffer(media)
		off, err := parseOffer(raw)
		if media {
			require.NoError(t, err)
			require.Nil(t, off.accepted("application"))
		} else {
			require.ErrorIs(t, err, ErrUnsupportedOffer)
		}
		on, err := parseOfferWithData(raw, cfg)
		require.NoError(t, err)
		require.True(t, on.data)
		require.NotNil(t, on.accepted("application"))
		for _, invalid := range []string{strings.Replace(raw, "sctp-port:5000", "sctp-port:6000", 1), strings.Replace(raw, "UDP/DTLS/SCTP", "DTLS/SCTP", 1), strings.Replace(raw, "max-message-size:65536", "max-message-size:bad", 1), raw + raw[strings.Index(raw, "m=application"):]} {
			_, err := parseOfferWithData(invalid, cfg)
			require.ErrorIs(t, err, ErrDataChannelOffer)
		}
	}
}
func TestDataChannelMarkerExistsBeforeHandshakeAndCannotResume(t *testing.T) {
	w, err := New(Config{DataChannel: &DataChannelConfig{Upstream: "ws://127.0.0.1/"}})
	require.NoError(t, err)
	defer w.Close()
	id, _, err := w.CreateSession(context.Background(), dataSDPOffer(false))
	require.NoError(t, err)
	raw, err := w.SnapshotSession(id)
	require.NoError(t, err)
	data, err := SnapshotIsDataSession(raw)
	require.NoError(t, err)
	require.True(t, data)
	require.Contains(t, string(raw), `"Version":9`)
	_, err = decodeSnapshot(raw)
	require.ErrorIs(t, err, ErrDataSessionReconnectRequired)
	_, err = w.ExportSession(id)
	require.ErrorIs(t, err, ErrDataSessionNotMovable)
	_, _, err = w.session(id).export()
	require.ErrorIs(t, err, ErrDataSessionNotMovable)
	require.Equal(t, 1, w.SessionCount())
}

func TestDataMarkerDoesNotClaimFutureMediaVersion(t *testing.T) {
	raw := []byte(`{"Version":9,"State":{"Version":9,"Data":false}}`)
	data, err := SnapshotIsDataSession(raw)
	require.NoError(t, err)
	require.False(t, data, "a future media envelope must not become a data session")
	_, err = decodeSnapshot(raw)
	require.ErrorIs(t, err, errStateVersion)
	require.NotErrorIs(t, err, ErrDataSessionReconnectRequired)
	for _, version := range []int{7, 8, 9, 42} {
		raw := []byte(fmt.Sprintf(`{"Version":%d,"State":{"Version":%d,"Data":true}}`, version, version))
		data, err := SnapshotIsDataSession(raw)
		require.NoError(t, err)
		require.True(t, data)
		_, err = decodeSnapshot(raw)
		require.ErrorIs(t, err, ErrDataSessionReconnectRequired)
	}
}

func TestDataSessionEndDoesNotWaitForUpstreamWriter(t *testing.T) {
	for _, closeWorker := range []bool{false, true} {
		t.Run(fmt.Sprint(closeWorker), func(t *testing.T) {
			w, err := New(Config{DataChannel: &DataChannelConfig{Upstream: "ws://127.0.0.1/"}})
			require.NoError(t, err)
			defer w.Close()
			id, _, err := w.CreateSession(context.Background(), dataSDPOffer(false))
			require.NoError(t, err)
			d := w.session(id).data
			// A writer can hold this serialization lock while its upstream write stalls.
			// Exercise the public end/close path without waiting on that writer.
			d.writeMu.Lock()
			defer d.writeMu.Unlock()
			done := make(chan error, 1)
			go func() {
				if closeWorker {
					done <- w.Close()
				} else {
					done <- w.EndSessionReason(id, "reconnect_required")
				}
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(100 * time.Millisecond):
				t.Fatal("session end blocked behind upstream writer")
			}
			require.Zero(t, w.SessionCount())
		})
	}
}
