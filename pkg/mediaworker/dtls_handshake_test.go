package mediaworker

import (
	"context"
	"encoding/binary"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/transport/v4/packetio"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// A lost-flight regression: the server's last flight (CCS epoch 0 + Finished epoch 1) is
// lost once. The worker completes its handshake and writes the first
// checkpoint (production order in connect). The real Pion client then
// retransmits flight 5 on its own timer and completes the handshake.
func TestLostServerFinalFlightCompletesAfterCheckpoint(t *testing.T) {
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	snap.State.DTLS.CallerFingerprint = certificateFingerprint(cert.Certificate[0])
	wire := &dtlsWireConn{packets: make(chan []byte, 256)}
	w := &Worker{conn: wire, localAddr: lease.Worker, cfg: Config{
		LoggerFactory: logging.NewDefaultLoggerFactory(), consentTimeout: time.Hour,
		Agent: (AgentConfig{}).defaults(), SnapshotLimits: (SnapshotLimits{}).defaults(),
		Relay: &RelayConfig{Owners: store},
	}}
	s := sessionFromState(w, snap.State)
	s.lease = lease
	t.Cleanup(func() { s.fenced.Store(true); s.close() })
	peer := &dtlsClientEndpoint{server: s.dtlsEndpoint, buffer: packetio.NewBuffer()}
	var dropped atomic.Bool
	wire.forward = func(p []byte) (int, error) {
		// Drop the first datagram that carries an epoch-1 record (flight 6).
		for q := p; len(q) >= 13; q = q[13+int(binary.BigEndian.Uint16(q[11:13])):] {
			if binary.BigEndian.Uint16(q[3:5]) == 1 && dropped.CompareAndSwap(false, true) {
				return len(p), nil
			}
			if 13+int(binary.BigEndian.Uint16(q[11:13])) > len(q) {
				break
			}
		}
		return peer.buffer.Write(p)
	}
	options, err := s.dtlsServerOptions()
	require.NoError(t, err)
	server, err := dtls.ServerWithOptions(s.dtlsEndpoint, s.dtlsEndpoint.RemoteAddr(), options...)
	require.NoError(t, err)
	s.dtlsConn = server
	client, err := dtls.ClientWithOptions(peer, peer.server.LocalAddr(), dtls.WithCertificates(cert),
		dtls.WithSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM), dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithInsecureSkipVerify(true), dtls.WithFlightInterval(200*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.HandshakeContext(ctx) }()
	require.NoError(t, server.HandshakeContext(ctx))
	require.True(t, dropped.Load(), "flight 6 was not dropped")
	_, err = s.startSRTP(server)
	require.NoError(t, err)
	require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional)) // connect(): first checkpoint right after the handshake
	err = <-clientDone
	t.Logf("guard drops=%d client handshake err=%v", s.worker.metrics.dtlsSequenceDrops.Load(), err)
	require.NoError(t, err, "caller never completes DTLS after one lost final flight")
}
