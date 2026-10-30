package mediaworker

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/require"
)

func TestReplayFollowsRenominatedCallerAddress(t *testing.T) {
	w := newTestWorker(t)
	w.cfg.consentTimeout = 5 * time.Second
	w.cfg.DisableResumePLI = true
	w.cfg.ReplayBitrate = 8_000
	w.cfg.ReplayMaxBurstDuration = 3 * time.Second
	id, answer, err := w.CreateSession(context.Background(), testOffer(testOfferAttrs{}))
	require.NoError(t, err)
	old, next := listenTestUDP(t), listenTestUDP(t)
	nominate := func(sock *net.UDPConn) {
		check, e := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(id+":"+testCallerUfrag), stun.RawAttribute{Type: stun.AttrUseCandidate}, stun.NewShortTermIntegrity(answerPwd(t, answer)), stun.Fingerprint)
		require.NoError(t, e)
		_, e = sock.WriteToUDPAddrPort(check.Raw, w.LocalAddr())
		require.NoError(t, e)
	}
	nominate(old)
	require.NoError(t, old.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, _, err = old.ReadFromUDP(make([]byte, 2048))
	require.NoError(t, err)
	s := w.session(id)
	s.mu.Lock()
	s.replaying = true
	s.mu.Unlock()
	packets := [][]byte{make([]byte, 1000), make([]byte, 1000)}
	packets[1][1] = 0x80
	done := make(chan struct{})
	go func() { s.sendReplay(packets); close(done) }()
	read := func(sock *net.UDPConn) []byte {
		require.NoError(t, sock.SetReadDeadline(time.Now().Add(3*time.Second)))
		b := make([]byte, 2048)
		n, _, e := sock.ReadFromUDP(b)
		require.NoError(t, e)
		return b[:n]
	}
	require.Len(t, read(old), 1000)
	nominate(next)
	require.True(t, stun.IsMessage(read(next)), "authenticated nomination completed between replay packets")
	require.Equal(t, packets[1], read(next), "each replay send must use the current caller address")
	<-done
}
