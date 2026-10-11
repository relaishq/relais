package mediaworker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/srtp/v3"
	"github.com/relais/internal/privateapi"
	"github.com/stretchr/testify/require"
)

func TestCheckpointReplayRecoveryPrivateAPISendsEncryptedPLI(t *testing.T) {
	w := newTestWorker(t)
	sink := listenTestUDP(t)
	state := sessionState{ID: "replay-recovery", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()}, Video: trackState{MID: "1", SSRC: 123, InboundSSRC: 456, PayloadType: 96, Anchored: true}}
	sess := sessionFromState(w, state)
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	caller := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	w.mu.Lock()
	w.sessions[state.ID] = sess
	w.mu.Unlock()
	server := httptest.NewServer(w.PrivateHandler())
	defer server.Close()
	for i := range 2 {
		require.NoError(t, privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/sessions/"+state.ID+"/keyframe", nil, nil, RemoteErrors))
		require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
		buf := make([]byte, 1500)
		n, _, err := sink.ReadFromUDP(buf)
		require.NoError(t, err)
		plain, err := caller.DecryptRTCP(nil, buf[:n], nil)
		require.NoError(t, err)
		packets, err := rtcp.Unmarshal(plain)
		require.NoError(t, err)
		require.Len(t, packets, 1)
		pli, ok := packets[0].(*rtcp.PictureLossIndication)
		require.True(t, ok)
		require.EqualValues(t, 456, pli.MediaSSRC)
		index, ok := sess.srtpOut.Index(123)
		require.True(t, ok)
		require.EqualValues(t, i+1, index, "requests never reuse an SRTCP index")
		sess.mu.Lock()
		require.False(t, sess.needsKeyframe)
		sess.mu.Unlock()
	}
	sess.fenced.Store(true)
	require.ErrorIs(t, w.RequestKeyframe(context.Background(), state.ID), ErrClosed)
	require.ErrorIs(t, w.RequestKeyframe(context.Background(), "missing"), ErrUnknownSession)
}
