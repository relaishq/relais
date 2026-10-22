package mediaworker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/dtls/v3/pkg/protocol/recordlayer"
	"github.com/pion/logging"
	"github.com/pion/transport/v4/packetio"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Capture the actual datagrams leaving the worker, below Pion and the endpoint
// guard. Reads belong to the session's DTLS endpoint, not this media socket.
type dtlsWireConn struct {
	packetConn
	packets chan []byte
	forward func([]byte) (int, error)
}

func (c *dtlsWireConn) WriteToUDPAddrPort(p []byte, _ netip.AddrPort) (int, error) {
	c.packets <- append([]byte(nil), p...)
	if c.forward != nil {
		return c.forward(p)
	}
	return len(p), nil
}

func dtlsFixture(t testing.TB) snapshot {
	t.Helper()
	data, err := os.ReadFile("testdata/v7-787127e.json")
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	return *snap
}

func dtlsCheckpointSession(t testing.TB, store sessionstore.Store, snap snapshot, lease sessionstore.Lease, opts ResumeOptions) (*session, *dtlsWireConn) {
	t.Helper()
	wire := &dtlsWireConn{packets: make(chan []byte, 256)}
	w := &Worker{conn: wire, localAddr: lease.Worker, cfg: Config{
		LoggerFactory: logging.NewDefaultLoggerFactory(), consentTimeout: time.Hour,
		Agent: (AgentConfig{}).defaults(), SnapshotLimits: (SnapshotLimits{}).defaults(),
		Relay: &RelayConfig{Owners: store},
	}}
	s := sessionFromState(w, snap.State)
	s.lease = lease
	_, err := s.resume(snap.DTLSConnection, opts)
	require.NoError(t, err)
	t.Cleanup(func() { s.fenced.Store(true); s.close() })
	return s, wire
}

func dtlsPacket(t *testing.T, wire *dtlsWireConn) []byte {
	t.Helper()
	select {
	case p := <-wire.packets:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not send a DTLS record")
		return nil
	}
}

func dtlsNonceInput(t *testing.T, p []byte) [8]byte {
	t.Helper()
	require.GreaterOrEqual(t, len(p), 13)
	var pair [8]byte
	copy(pair[:], p[3:11])
	return pair
}

func TestCrashRestoreNeverRepeatsDTLSSequence(t *testing.T) {
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	old, oldWire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	require.NoError(t, old.checkpoint.persist(old.ctx, checkpointOptional))
	stale, err := store.GetState(context.Background(), old.id)
	require.NoError(t, err)
	var saved snapshot
	require.NoError(t, json.Unmarshal(stale, &saved))
	seen := make(map[[8]byte]bool)
	record := func(wire *dtlsWireConn) {
		p := dtlsPacket(t, wire)
		pair := dtlsNonceInput(t, p)
		require.Equal(t, byte(0), pair[0])
		require.Equal(t, byte(1), pair[1])
		require.False(t, seen[pair], "DTLS epoch/sequence reused on wire: %x", pair)
		seen[pair] = true
	}
	// Pion allocates these records after the last acknowledged copy. Distinct
	// test payloads make nonce reuse observable beyond identical close alerts.
	for range 3 {
		_, err := old.dtlsConn.Write([]byte("old-owner"))
		require.NoError(t, err)
		record(oldWire)
	}
	require.NoError(t, old.dtlsConn.Close())
	record(oldWire) // automatic Pion close alert, through the production endpoint
	lease, err = store.Transfer(context.Background(), lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
	require.NoError(t, err)
	restored, wire := dtlsCheckpointSession(t, store, saved, lease, ResumeOptions{Kind: agent.Takeover, SequenceMargin: 8192})
	require.NoError(t, restored.checkpoint.persist(restored.ctx, checkpointOptional)) // production adoption barrier
	_, err = restored.dtlsConn.Write([]byte("replacement"))
	require.NoError(t, err)
	record(wire)
	require.NoError(t, restored.dtlsConn.Close())
	record(wire)
}

// An in-memory caller transport keeps the real Pion handshake and automatic
// retransmission paths, without UDP timing or a background checkpoint loop.
type dtlsClientEndpoint struct {
	server *dtlsEndpoint
	buffer *packetio.Buffer
}

func (c *dtlsClientEndpoint) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.buffer.Read(p)
	return n, c.server.LocalAddr(), err
}
func (c *dtlsClientEndpoint) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.server.deliver(p)
	return len(p), nil
}
func (c *dtlsClientEndpoint) LocalAddr() net.Addr               { return c.server.RemoteAddr() }
func (c *dtlsClientEndpoint) Close() error                      { return c.buffer.Close() }
func (c *dtlsClientEndpoint) SetDeadline(t time.Time) error     { return c.buffer.SetReadDeadline(t) }
func (c *dtlsClientEndpoint) SetReadDeadline(t time.Time) error { return c.buffer.SetReadDeadline(t) }
func (c *dtlsClientEndpoint) SetWriteDeadline(time.Time) error  { return nil }

func dtlsHandshakenSession(t *testing.T, store sessionstore.Store, lease sessionstore.Lease) (*session, *dtlsWireConn) {
	t.Helper()
	snap := dtlsFixture(t)
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
	wire.forward = peer.buffer.Write
	options, err := s.dtlsServerOptions()
	require.NoError(t, err)
	server, err := dtls.ServerWithOptions(s.dtlsEndpoint, s.dtlsEndpoint.RemoteAddr(), options...)
	require.NoError(t, err)
	s.dtlsConn = server
	client, err := dtls.ClientWithOptions(peer, peer.server.LocalAddr(), dtls.WithCertificates(cert), dtls.WithSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM), dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret), dtls.WithInsecureSkipVerify(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(ctx) }()
	require.NoError(t, client.HandshakeContext(ctx))
	require.NoError(t, <-done)
	_, err = s.startSRTP(server)
	require.NoError(t, err)
	for len(wire.packets) > 0 {
		<-wire.packets
	}
	return s, wire
}

type dtlsPendingReplyStore struct {
	sessionstore.Store
	committed chan struct{}
	reply     chan struct{}
	result    error
}

func (s *dtlsPendingReplyStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if err := s.Store.PutState(ctx, lease, data); err != nil {
		return err
	}
	close(s.committed)
	select {
	case <-s.reply:
		return s.result
	case <-ctx.Done():
		return &sessionstore.TransientError{Op: "put_state", Err: ctx.Err()}
	}
}

// A retransmitted epoch-zero handshake fragment with a new record sequence
// makes Pion resend its last server flight, including the encrypted Finished.
// It is the actual automatic path; the test never writes DTLS application data.
func dtlsRetransmitRequest(sequence uint64) []byte {
	p := make([]byte, 25)
	p[0], p[1], p[2] = 22, 0xfe, 0xfd
	binary.BigEndian.PutUint64(p[3:11], sequence) // epoch 0 plus uint48 sequence
	binary.BigEndian.PutUint16(p[11:13], 12)
	p[13] = 1 // ClientHello; old handshake message sequence 0, empty fragment
	return p
}

func TestCrashRestoreAfterLostFirstCheckpointReplyNeverRepeatsDTLSSequence(t *testing.T) {
	base := sessionstore.NewMemory()
	store := &dtlsPendingReplyStore{Store: base, committed: make(chan struct{}), reply: make(chan struct{}), result: &sessionstore.TransientError{Op: "put_state", Err: context.DeadlineExceeded}}
	snap := dtlsFixture(t)
	lease, err := base.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	old, wire := dtlsHandshakenSession(t, store, lease)
	result := make(chan error, 1)
	go func() { result <- old.checkpoint.persist(old.ctx, checkpointOptional) }()
	select {
	case <-store.committed:
	case <-time.After(2 * time.Second):
		t.Fatal("first checkpoint did not commit")
	}
	// Drive more than the reservation through Pion's automatic retransmitter.
	// Count both sent and rejected datagrams so this test also completes when
	// the guard works. No small callback deadline or silence timer is needed.
	for i := range 65 {
		old.dtlsEndpoint.deliver(dtlsRetransmitRequest(uint64(1000 + i)))
	}
	require.Eventually(t, func() bool {
		return uint64(len(wire.packets))+old.worker.metrics.dtlsSequenceDrops.Load() >= 65
	}, 5*time.Second, time.Millisecond, "Pion must process all retransmission requests")
	seen := make(map[[8]byte]bool)
	for len(wire.packets) > 0 {
		p := <-wire.packets
		for len(p) > 0 {
			pair := dtlsNonceInput(t, p)
			if pair[0] == 0 && pair[1] == 1 {
				seen[pair] = true
			}
			size := 13 + int(binary.BigEndian.Uint16(p[11:13]))
			require.LessOrEqual(t, size, len(p))
			p = p[size:]
		}
	}
	require.Len(t, seen, 64, "the provisional bound must allow encrypted Finished 1..64")
	for sequence := uint64(1); sequence <= 64; sequence++ {
		var pair [8]byte
		binary.BigEndian.PutUint64(pair[:], uint64(1)<<48|sequence)
		require.True(t, seen[pair], "encrypted Finished %d must be sent", sequence)
	}
	var blocked [8]byte
	binary.BigEndian.PutUint64(blocked[:], uint64(1)<<48|65)
	require.False(t, seen[blocked], "encrypted Finished 65 must be dropped")
	require.EqualValues(t, 1, old.worker.metrics.dtlsSequenceDrops.Load())
	close(store.reply)
	require.ErrorIs(t, <-result, sessionstore.ErrTransient)
	stale, err := base.GetState(context.Background(), old.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(stale)
	require.NoError(t, err)
	old.fenced.Store(true)
	old.close()
	lease, err = base.Transfer(context.Background(), lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
	require.NoError(t, err)
	restored, restoredWire := dtlsCheckpointSession(t, base, *saved, lease, ResumeOptions{Kind: agent.Takeover, SequenceMargin: 8192})
	require.NoError(t, restored.checkpoint.persist(restored.ctx, checkpointOptional))
	require.NoError(t, restored.dtlsConn.Close())
	pair := dtlsNonceInput(t, dtlsPacket(t, restoredWire))
	require.False(t, seen[pair], "DTLS epoch/sequence reused after lost FIRST checkpoint reply: %x", pair)
}

func TestRepeatedTakeoversNeverRepeatDTLSSequence(t *testing.T) {
	store := sessionstore.NewMemory()
	saved := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), saved.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	seen := make(map[[8]byte]bool)
	for generation, expected := range []uint64{1, 65, 129, 193} {
		opts := ResumeOptions{}
		if generation > 0 {
			opts = ResumeOptions{Kind: agent.Takeover, SequenceMargin: 1}
		}
		s, wire := dtlsCheckpointSession(t, store, saved, lease, opts)
		require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
		data, err := store.GetState(context.Background(), s.id)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &saved))
		for range 3 {
			_, err := s.dtlsConn.Write([]byte("tail"))
			require.NoError(t, err)
			pair := dtlsNonceInput(t, dtlsPacket(t, wire))
			require.False(t, seen[pair], "repeated takeover reused %x", pair)
			seen[pair] = true
		}
		require.NoError(t, s.dtlsConn.Close())
		pair := dtlsNonceInput(t, dtlsPacket(t, wire))
		require.Equal(t, expected+3, binary.BigEndian.Uint64(pair[:])&0xffffffffffff)
		require.False(t, seen[pair])
		seen[pair] = true
		require.Zero(t, s.worker.metrics.dtlsSequenceDrops.Load())
		if generation < 3 {
			lease, err = store.Transfer(context.Background(), lease, netip.AddrPortFrom(lease.Worker.Addr(), uint16(10001+generation)), time.Hour)
			require.NoError(t, err)
		}
	}
}

type dtlsWriteReplyStore struct {
	sessionstore.Store
	result error
	commit bool
}

func (s *dtlsWriteReplyStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if s.commit {
		if err := s.Store.PutState(ctx, lease, data); err != nil {
			return err
		}
	}
	return s.result
}

func TestDTLSUnknownCheckpointDoesNotAdvanceBound(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			base := sessionstore.NewMemory()
			store := &dtlsWriteReplyStore{Store: base, commit: true}
			saved := dtlsFixture(t)
			lease, err := base.Claim(context.Background(), saved.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
			require.NoError(t, err)
			s, wire := dtlsCheckpointSession(t, store, saved, lease, ResumeOptions{})
			require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
			seen := make(map[[8]byte]bool)
			for range 3 {
				_, err := s.dtlsConn.Write([]byte("before-unknown"))
				require.NoError(t, err)
				seen[dtlsNonceInput(t, dtlsPacket(t, wire))] = true
			}
			store.commit = committed
			store.result = &sessionstore.TransientError{Op: "put_state", Err: context.DeadlineExceeded}
			require.ErrorIs(t, s.checkpoint.persist(s.ctx, checkpointOptional), sessionstore.ErrTransient)
			// The current copy captured next=4, but no reply acknowledged it. All
			// records >=65 must still be refused, even if that copy committed.
			for range 61 {
				_, err := s.dtlsConn.Write([]byte("uncertain-tail"))
				require.NoError(t, err)
				seen[dtlsNonceInput(t, dtlsPacket(t, wire))] = true
			}
			_, err = s.dtlsConn.Write([]byte("outside-old-bound"))
			require.NoError(t, err)
			require.Empty(t, wire.packets)
			require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load())
			data, err := base.GetState(context.Background(), s.id)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &saved))
			s.fenced.Store(true)
			s.close()
			lease, err = base.Transfer(context.Background(), lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
			require.NoError(t, err)
			restored, restoredWire := dtlsCheckpointSession(t, base, saved, lease, ResumeOptions{Kind: agent.Takeover, SequenceMargin: 8192})
			require.NoError(t, restored.checkpoint.persist(restored.ctx, checkpointOptional))
			require.NoError(t, restored.dtlsConn.Close())
			pair := dtlsNonceInput(t, dtlsPacket(t, restoredWire))
			require.False(t, seen[pair], "uncertain checkpoint led to repeated nonce input %x", pair)
			expected := uint64(65)
			if committed {
				expected = 68
			}
			require.Equal(t, expected, binary.BigEndian.Uint64(pair[:])&0xffffffffffff)
		})
	}
}

func TestDTLSGuardRequestsCheckpointAndResumesAfterAcknowledgment(t *testing.T) {
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	// An observation alone is not a checkpoint write attempt.
	_, err = s.checkpoint.capture(checkpointLive, nil)
	require.NoError(t, err)
	s.mu.Lock()
	require.True(t, s.checkpoint.allowDTLSLocked(dtlsHeaderRecord(22, 1, 1000)), "read-only capture cannot arm a DTLS bound")
	s.mu.Unlock()
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointOptional))
	events := observeCheckpointRequests(t, s)
	for range 64 {
		_, err := s.dtlsConn.Write([]byte("within-reservation"))
		require.NoError(t, err)
		dtlsPacket(t, wire)
	}
	_, err = s.dtlsConn.Write([]byte("past-bound"))
	require.NoError(t, err)
	require.Empty(t, wire.packets)
	require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load())
	require.Contains(t, metricsBody(s.worker), "relais_worker_dtls_sequence_guard_drops_total 1\n")
	requireCheckpointRequest(t, events, true)
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointOptional))
	_, err = s.dtlsConn.Write([]byte("acknowledged-new-bound"))
	require.NoError(t, err)
	pair := dtlsNonceInput(t, dtlsPacket(t, wire))
	require.EqualValues(t, 66, binary.BigEndian.Uint64(pair[:])&0xffffffffffff)
	require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load())
}

func TestDTLSGuardRequestsOneCheckpointPerBound(t *testing.T) {
	base := sessionstore.NewMemory()
	store := &dtlsWriteReplyStore{Store: base, commit: true}
	snap := dtlsFixture(t)
	lease, err := base.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, _ := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointOptional))
	events := observeCheckpointRequests(t, s)
	drop := func(want bool) {
		t.Helper()
		_, err := s.dtlsEndpoint.WriteTo(dtlsHeaderRecord(22, 1, 1000), nil)
		require.NoError(t, err)
		requireCheckpointRequest(t, events, want)
	}
	drop(true)
	drop(false) // draining the wakeup must not permit caller-driven writes
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointOptional))
	drop(false) // an acknowledgment of the same bound does not rearm it
	store.result = &sessionstore.TransientError{Op: "put_state", Err: context.DeadlineExceeded}
	_, err = s.dtlsConn.Write([]byte("advance-capture"))
	require.NoError(t, err)
	require.ErrorIs(t, s.checkpoint.persist(context.Background(), checkpointOptional), sessionstore.ErrTransient)
	drop(false) // uncertain commits never advance the bound or reset its latch
	store.result = nil
	require.NoError(t, s.checkpoint.persist(context.Background(), checkpointOptional))
	drop(true) // an acknowledged new bound permits one new request
	drop(false)
	require.EqualValues(t, 6, s.worker.metrics.dtlsSequenceDrops.Load())
}

func TestDTLSAutomaticCloseAlertUsesSequenceGuard(t *testing.T) {
	for _, tail := range []int{0, 64} {
		t.Run(fmt.Sprintf("tail=%d", tail), func(t *testing.T) {
			store := sessionstore.NewMemory()
			snap := dtlsFixture(t)
			lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
			require.NoError(t, err)
			s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
			require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
			for range tail {
				_, err := s.dtlsConn.Write([]byte("tail"))
				require.NoError(t, err)
				dtlsPacket(t, wire)
			}
			require.NoError(t, s.dtlsConn.Close())
			if tail == 0 {
				require.Equal(t, byte(21), dtlsPacket(t, wire)[0])
				require.Zero(t, s.worker.metrics.dtlsSequenceDrops.Load())
			} else {
				require.Empty(t, wire.packets, "out-of-reservation close alert escaped")
				require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load())
			}
		})
	}
}

func TestDTLSAutomaticHandshakeRetransmissionUsesSequenceGuard(t *testing.T) {
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsHandshakenSession(t, store, lease)
	require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
	for i := range 65 {
		s.dtlsEndpoint.deliver(dtlsRetransmitRequest(uint64(1000 + i)))
	}
	require.Eventually(t, func() bool {
		return uint64(len(wire.packets))+s.worker.metrics.dtlsSequenceDrops.Load() >= 65
	}, 5*time.Second, time.Millisecond)
	require.Len(t, wire.packets, 64, "plaintext ChangeCipherSpec must not block encrypted Finished within the bound")
	for sequence := uint64(1); sequence <= 64; sequence++ {
		p := dtlsPacket(t, wire)
		require.Equal(t, byte(20), p[0], "retransmitted flight starts with ChangeCipherSpec")
		require.Zero(t, binary.BigEndian.Uint16(p[3:5]))
		size := 13 + int(binary.BigEndian.Uint16(p[11:13]))
		require.Less(t, size, len(p), "Finished must follow in the same datagram")
		finished := p[size:]
		require.Equal(t, byte(22), finished[0])
		pair := dtlsNonceInput(t, finished)
		require.Equal(t, uint64(1)<<48|sequence, binary.BigEndian.Uint64(pair[:]))
	}
	require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load(), "encrypted Finished 65 must be dropped")
}

func dtlsHeaderRecord(contentType byte, epoch uint16, sequence uint64) []byte {
	p := make([]byte, 13)
	p[0], p[1], p[2] = contentType, 0xfe, 0xfd
	binary.BigEndian.PutUint64(p[3:11], sequence)
	binary.BigEndian.PutUint16(p[3:5], epoch)
	return p
}

func TestDTLSGuardChecksWholeDatagram(t *testing.T) {
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
	plaintext := dtlsHeaderRecord(22, 0, recordlayer.MaxSequenceNumber)
	_, err = s.dtlsEndpoint.WriteTo(plaintext, nil)
	require.NoError(t, err)
	require.Equal(t, plaintext, dtlsPacket(t, wire), "plaintext epoch zero has no nonce reservation")
	for _, contentType := range []byte{21, 22, 23} {
		valid := dtlsHeaderRecord(contentType, 1, 64)
		_, err := s.dtlsEndpoint.WriteTo(valid, nil)
		require.NoError(t, err)
		require.Equal(t, valid, dtlsPacket(t, wire))
		blocked := dtlsHeaderRecord(contentType, 1, 65)
		_, err = s.dtlsEndpoint.WriteTo(append(valid, blocked...), nil)
		require.NoError(t, err)
		require.Empty(t, wire.packets, "valid first record must not hide an out-of-bound second record")
	}
	malformed := dtlsHeaderRecord(21, 1, 1)
	binary.BigEndian.PutUint16(malformed[11:13], 1)
	for _, p := range [][]byte{malformed, make([]byte, 12), dtlsHeaderRecord(25, 1, 1), dtlsHeaderRecord(21, 2, 1)} {
		_, err := s.dtlsEndpoint.WriteTo(p, nil)
		require.NoError(t, err)
		require.Empty(t, wire.packets)
	}
	require.EqualValues(t, 7, s.worker.metrics.dtlsSequenceDrops.Load())
}

func TestDTLSAdapterPreservesPinnedState(t *testing.T) {
	manifest, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	require.Contains(t, string(manifest), "\tgithub.com/pion/dtls/v3 v3.1.8\n", "update the serialization adapter contract when upgrading Pion")
	source, err := decodeDTLSSerializedState(dtlsFixture(t).DTLSConnection)
	require.NoError(t, err)
	source.LocalEpoch, source.RemoteEpoch, source.SequenceNumber = 2, 2, 255
	source.IdentityHint = []byte("identity")
	source.SessionID = []byte("session")
	source.LocalConnectionID = []byte("local")
	source.RemoteConnectionID = []byte("remote")
	source.NegotiatedProtocol = "test-protocol"
	source.IsClient = true
	var original bytes.Buffer
	require.NoError(t, gob.NewEncoder(&original).Encode(source))
	var state dtls.State
	require.NoError(t, state.UnmarshalBinary(original.Bytes()))
	keys, err := state.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 60)
	require.NoError(t, err)
	advanced, err := advanceDTLSSequence(original.Bytes())
	require.NoError(t, err)
	require.NoError(t, state.UnmarshalBinary(advanced))
	reencoded, err := state.MarshalBinary()
	require.NoError(t, err)
	actual, err := decodeDTLSSerializedState(reencoded)
	require.NoError(t, err)
	source.SequenceNumber = 319
	require.Equal(t, source, actual, "all setup and changing fields except next sequence must survive")
	restoredKeys, err := state.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 60)
	require.NoError(t, err)
	require.Equal(t, keys, restoredKeys)
}

func TestDTLSSequenceExhaustionRefusesRestore(t *testing.T) {
	for _, tc := range []struct {
		next uint64
		want error
	}{
		{recordlayer.MaxSequenceNumber - 64, nil},
		{recordlayer.MaxSequenceNumber - 63, ErrDTLSSequenceExhausted},
		{recordlayer.MaxSequenceNumber + 1, ErrDTLSSequenceExhausted},
		{recordlayer.MaxSequenceNumber + 2, errBadState},
	} {
		source, err := decodeDTLSSerializedState(dtlsFixture(t).DTLSConnection)
		require.NoError(t, err)
		source.SequenceNumber = tc.next
		var encoded bytes.Buffer
		require.NoError(t, gob.NewEncoder(&encoded).Encode(source))
		actual, err := advanceDTLSSequence(encoded.Bytes())
		if tc.want == nil {
			require.NoError(t, err)
			decoded, err := decodeDTLSSerializedState(actual)
			require.NoError(t, err)
			require.EqualValues(t, recordlayer.MaxSequenceNumber, decoded.SequenceNumber)
		} else {
			require.ErrorIs(t, err, tc.want)
		}
	}
	_, err := advanceDTLSSequence([]byte("invalid"))
	require.ErrorIs(t, err, errBadState)
	require.ErrorIs(t, RemoteErrors["dtls_exhausted"], ErrDTLSSequenceExhausted)
}

func TestDTLSFirstWriteBoundSurvivesRetries(t *testing.T) {
	base := sessionstore.NewMemory()
	store := &dtlsWriteReplyStore{Store: base, commit: false, result: &sessionstore.TransientError{Op: "put_state", Err: context.DeadlineExceeded}}
	snap := dtlsFixture(t)
	lease, err := base.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	require.ErrorIs(t, s.checkpoint.persist(s.ctx, checkpointOptional), sessionstore.ErrTransient)
	for range 64 {
		_, err := s.dtlsConn.Write([]byte("provisional-tail"))
		require.NoError(t, err)
		dtlsPacket(t, wire)
	}
	// A failed retry captures a larger N but must retain the FIRST bound.
	require.ErrorIs(t, s.checkpoint.persist(s.ctx, checkpointOptional), sessionstore.ErrTransient)
	_, err = s.dtlsConn.Write([]byte("past-provisional-bound"))
	require.NoError(t, err)
	require.Empty(t, wire.packets)
	require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load())
	store.commit, store.result = true, nil
	require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
	_, err = s.dtlsConn.Write([]byte("after-first-acknowledgment"))
	require.NoError(t, err)
	pair := dtlsNonceInput(t, dtlsPacket(t, wire))
	require.EqualValues(t, 66, binary.BigEndian.Uint64(pair[:])&0xffffffffffff)
}

func TestDTLSAcknowledgmentUsesCapturedSequence(t *testing.T) {
	base := sessionstore.NewMemory()
	store := &dtlsPendingReplyStore{Store: base, committed: make(chan struct{}), reply: make(chan struct{})}
	snap := dtlsFixture(t)
	lease, err := base.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
	result := make(chan error, 1)
	go func() { result <- s.checkpoint.persist(s.ctx, checkpointOptional) }()
	select {
	case <-store.committed:
	case <-time.After(2 * time.Second):
		t.Fatal("write did not commit")
	}
	for range 3 {
		_, err := s.dtlsConn.Write([]byte("while-write-is-in-flight"))
		require.NoError(t, err)
		dtlsPacket(t, wire)
	}
	close(store.reply)
	require.NoError(t, <-result)
	// The acknowledged next sequence is 1, even though the live next is 4.
	for range 61 {
		_, err := s.dtlsConn.Write([]byte("acknowledged-tail"))
		require.NoError(t, err)
		dtlsPacket(t, wire)
	}
	_, err = s.dtlsConn.Write([]byte("outside-captured-bound"))
	require.NoError(t, err)
	require.Empty(t, wire.packets)
	require.EqualValues(t, 1, s.worker.metrics.dtlsSequenceDrops.Load())
}

func TestPlannedRestoreKeepsExactDTLSSequence(t *testing.T) {
	store := sessionstore.NewMemory()
	snap := dtlsFixture(t)
	lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
	require.NoError(t, err)
	s, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{Kind: agent.PlannedMove})
	require.NoError(t, s.checkpoint.persist(s.ctx, checkpointOptional))
	require.NoError(t, s.dtlsConn.Close())
	pair := dtlsNonceInput(t, dtlsPacket(t, wire))
	require.EqualValues(t, 1, binary.BigEndian.Uint64(pair[:])&0xffffffffffff)
}
