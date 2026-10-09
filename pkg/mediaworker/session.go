package mediaworker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/fingerprint"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

const (
	// sessionIDLength is also the length of the worker's ICE ufrag: the ufrag
	// is the session ID, so whoever reads a STUN USERNAME (the worker today,
	// a relay later) knows which session a caller's packets belong to.
	sessionIDLength = 16
	icePwdLength    = 32

	receiveMTU = 8192

	// replayWindow matches Pion's default SRTP/SRTCP replay window.
	replayWindow = 64
)

var errNoSRTPProfile = errors.New("mediaworker: DTLS negotiated no supported SRTP protection profile")

// supportedSRTPProfiles are offered to the caller in the DTLS use_srtp
// extension, in preference order.
var supportedSRTPProfiles = []dtls.SRTPProtectionProfile{
	dtls.SRTP_AEAD_AES_128_GCM,
	dtls.SRTP_AEAD_AES_256_GCM,
	dtls.SRTP_AES128_CM_HMAC_SHA1_80,
	dtls.SRTP_AES128_CM_HMAC_SHA1_32,
}

// session is one caller's WebRTC connection on this media worker.
//
// state is the session state as one plain value (see sessionState).
// dtlsConn, srtpIn and srtpOut are caches built from it. mu is the session's
// one lock: it guards state and the caches, and every packet the session
// sends or receives is processed under it, so a snapshot taken under mu is
// atomic with the counters.
type session struct {
	id     string // state.ID; never changes
	worker *Worker
	log    logging.LeveledLogger

	mu       sync.Mutex
	state    sessionState
	dtlsConn *dtls.Conn
	srtpIn   *srtp.Context // decrypts caller to worker
	srtpOut  *srtp.Context // encrypts worker to caller

	// Runtime plumbing, rebuilt by a worker that resumes the session.
	dtlsEndpoint  *dtlsEndpoint
	nominated     chan struct{} // closed when the caller first nominates an address
	nominatedOnce sync.Once
	consent       *time.Timer // closes the session when consent checks stop

	// Scratch buffers for packet processing; guarded by mu.
	rtcpBuf    []byte
	decryptBuf []byte
	plainBuf   []byte
	encryptBuf []byte

	// ctx is the session's lifetime; close cancels it.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// newSession creates a session for an offer: the session state with fresh
// ICE credentials, a fresh DTLS certificate and the outbound echo track. The
// session answers the caller's ICE checks as soon as the worker registers it.
func newSession(w *Worker, offer *remoteOffer) (*session, error) {
	id, err := randomString(sessionIDLength)
	if err != nil {
		return nil, err
	}
	pwd, err := randomString(icePwdLength)
	if err != nil {
		return nil, err
	}
	certDER, keyDER, err := newCertificate()
	if err != nil {
		return nil, err
	}
	audio, err := newTrackState("audio", offer.audio())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		id:     id,
		worker: w,
		log:    w.cfg.LoggerFactory.NewLogger("session"),
		state: sessionState{
			Version: sessionStateVersion,
			ID:      id,
			ICE: iceState{
				LocalUfrag:  id,
				LocalPwd:    pwd,
				RemoteUfrag: offer.iceUfrag,
				RemotePwd:   offer.icePwd,
			},
			DTLS: dtlsState{
				Certificate:           certDER,
				PrivateKey:            keyDER,
				CallerFingerprintHash: offer.fingerprintHash,
				CallerFingerprint:     offer.fingerprint,
			},
			SRTP:  srtpState{Inbound: make(map[uint32]uint64)},
			Audio: audio,
		},
		nominated:  make(chan struct{}),
		rtcpBuf:    make([]byte, receiveMTU),
		decryptBuf: make([]byte, receiveMTU),
		plainBuf:   make([]byte, receiveMTU),
		encryptBuf: make([]byte, receiveMTU),
		ctx:        ctx,
		cancel:     cancel,
	}
	sess.dtlsEndpoint = newDTLSEndpoint(sess)
	sess.consent = time.AfterFunc(w.cfg.consentTimeout, sess.consentExpired)

	return sess, nil
}

// answerParams is the session's half of the SDP answer.
func (s *session) answerParams() answerParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	return answerParams{
		iceUfrag:    s.state.ICE.LocalUfrag,
		icePwd:      s.state.ICE.LocalPwd,
		fingerprint: certificateFingerprint(s.state.DTLS.Certificate),
		candidate:   s.worker.MediaAddr(),
		audio:       s.state.Audio,
	}
}

// run drives the session after the answer is sent: it waits for the caller
// to nominate an address, runs the DTLS handshake as the DTLS server, derives
// the SRTP keys and keeps the session alive until the caller hangs up, its
// consent expires or the session is closed.
func (s *session) run() {
	defer s.close()

	connectCtx, cancel := context.WithTimeout(s.ctx, s.worker.cfg.ConnectTimeout)
	defer cancel()

	select {
	case <-s.nominated:
	case <-connectCtx.Done():
		s.log.Warnf("session %s: caller did not complete ICE: %v", s.id, connectCtx.Err())

		return
	}

	options, err := s.dtlsServerOptions()
	if err != nil {
		s.log.Warnf("session %s: DTLS options: %v", s.id, err)

		return
	}
	dtlsConn, err := dtls.ServerWithOptions(s.dtlsEndpoint, s.dtlsEndpoint.RemoteAddr(), options...)
	if err != nil {
		s.log.Warnf("session %s: create DTLS server: %v", s.id, err)

		return
	}
	// close may already have run (and found no DTLS connection to close), so
	// run closes the connection it created too. A second Close is harmless.
	defer func() { _ = dtlsConn.Close() }()
	s.mu.Lock()
	s.dtlsConn = dtlsConn
	s.mu.Unlock()

	if err := dtlsConn.HandshakeContext(connectCtx); err != nil {
		s.log.Warnf("session %s: DTLS handshake: %v", s.id, err)

		return
	}
	if err := s.startSRTP(dtlsConn); err != nil {
		s.log.Warnf("session %s: start SRTP: %v", s.id, err)

		return
	}
	s.log.Infof("session %s: established with %s", s.id, s.dtlsEndpoint.RemoteAddr())

	// WebRTC without data channels carries no DTLS application data, but the
	// DTLS connection still has to be read so alerts are processed: a
	// close_notify from the caller ends the session.
	buf := make([]byte, receiveMTU)
	for {
		if _, err := dtlsConn.Read(buf); err != nil {
			s.log.Debugf("session %s: DTLS closed: %v", s.id, err)

			return
		}
	}
}

func (s *session) dtlsServerOptions() ([]dtls.ServerOption, error) {
	s.mu.Lock()
	certDER, keyDER := s.state.DTLS.Certificate, s.state.DTLS.PrivateKey
	s.mu.Unlock()

	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}

	return []dtls.ServerOption{
		dtls.WithCertificates(cert),
		dtls.WithSRTPProtectionProfiles(supportedSRTPProfiles...),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithClientAuth(dtls.RequireAnyClientCert),
		// WebRTC certificates are self-signed; the caller is authenticated by
		// the fingerprint from its offer instead, in verifyCallerCertificate.
		dtls.WithInsecureSkipVerify(true),
		dtls.WithVerifyPeerCertificate(s.verifyCallerCertificate),
		dtls.WithLoggerFactory(s.worker.cfg.LoggerFactory),
	}, nil
}

// verifyCallerCertificate checks the caller's DTLS certificate against the
// fingerprint in its SDP offer.
func (s *session) verifyCallerCertificate(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("mediaworker: caller sent no DTLS certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}

	s.mu.Lock()
	hashName, want := s.state.DTLS.CallerFingerprintHash, s.state.DTLS.CallerFingerprint
	s.mu.Unlock()

	hash, err := fingerprint.HashFromString(hashName)
	if err != nil {
		return err
	}
	got, err := fingerprint.Fingerprint(cert, hash)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return errors.New("mediaworker: caller DTLS certificate does not match the offer's fingerprint")
	}

	return nil
}

// startSRTP derives the session's SRTP keys from the DTLS connection
// (RFC 5764) and builds the inbound and outbound SRTP contexts. The keys are
// not part of the session state: they can always be derived again from the
// DTLS connection.
func (s *session) startSRTP(conn *dtls.Conn) error {
	dtlsProfile, ok := conn.SelectedSRTPProtectionProfile()
	if !ok {
		return errNoSRTPProfile
	}
	profile, err := srtpProfile(dtlsProfile)
	if err != nil {
		return err
	}

	connState, ok := conn.ConnectionState()
	if !ok {
		return errors.New("mediaworker: DTLS connection state unavailable")
	}
	config := srtp.Config{Profile: profile}
	if err := config.ExtractSessionKeysFromDTLS(&connState, false); err != nil {
		return err
	}

	in, err := srtp.CreateContext(config.Keys.RemoteMasterKey, config.Keys.RemoteMasterSalt, profile,
		srtp.SRTPReplayProtection(replayWindow), srtp.SRTCPReplayProtection(replayWindow))
	if err != nil {
		return err
	}
	out, err := srtp.CreateContext(config.Keys.LocalMasterKey, config.Keys.LocalMasterSalt, profile)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.state.SRTP.Profile = profile
	s.srtpIn = in
	s.srtpOut = out
	s.mu.Unlock()

	return nil
}

// handlePacket demultiplexes a non-STUN packet from the caller (RFC 7983):
// DTLS records go to the DTLS connection, SRTP and SRTCP are handled here.
func (s *session) handlePacket(pkt []byte) {
	switch {
	case isDTLS(pkt):
		s.dtlsEndpoint.deliver(pkt)
	case isRTCP(pkt):
		s.handleRTCP(pkt)
	case isRTP(pkt):
		s.handleRTP(pkt)
	}
}

// handleRTP decrypts a packet from the caller and echoes it back on the
// matching outbound track.
func (s *session) handleRTP(pkt []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srtpIn == nil {
		return // SRTP keys are not ready yet.
	}

	// A successful decryption advances the inbound SRTP context's rollover
	// counter and replay window, so the session state records the packet's
	// index at once, from the header the context authenticated. Nothing that
	// can fail may sit between the two, or a snapshot would carry an index
	// behind the context's.
	var header rtp.Header
	plain, err := s.srtpIn.DecryptRTP(s.decryptBuf, pkt, &header)
	if err != nil {
		s.log.Debugf("session %s: drop SRTP packet: %v", s.id, err)

		return
	}
	s.state.SRTP.noteInbound(header.SSRC, header.SequenceNumber)

	var in rtp.Packet
	if err := in.Unmarshal(plain); err != nil {
		s.log.Debugf("session %s: drop RTP packet: %v", s.id, err)

		return
	}

	track := &s.state.Audio
	if in.PayloadType != track.PayloadType {
		return
	}
	header, ok := track.rewrite(&in.Header)
	if !ok {
		return
	}

	out := rtp.Packet{Header: header, Payload: in.Payload}
	n, err := out.MarshalTo(s.plainBuf)
	if err != nil {
		s.log.Debugf("session %s: marshal echo packet: %v", s.id, err)

		return
	}
	encrypted, err := s.srtpOut.EncryptRTP(s.encryptBuf, s.plainBuf[:n], nil)
	if err != nil {
		s.log.Warnf("session %s: encrypt echo packet: %v", s.id, err)

		return
	}
	s.encryptBuf = encrypted[:cap(encrypted)]
	track.noteSent(&header)

	if _, err := s.worker.send(encrypted, s.state.ICE.RemoteAddr); err != nil {
		s.log.Debugf("session %s: send echo packet: %v", s.id, err)
	}
}

// handleRTCP decrypts the caller's RTCP so the inbound SRTCP context stays
// current. The worker does not act on RTCP yet.
func (s *session) handleRTCP(pkt []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srtpIn == nil {
		return
	}
	if _, err := s.srtpIn.DecryptRTCP(s.rtcpBuf, pkt, nil); err != nil {
		s.log.Debugf("session %s: drop SRTCP packet: %v", s.id, err)
	}
}

func (s *session) consentExpired() {
	s.log.Infof("session %s: no ICE consent check from the nominated address for %s, closing",
		s.id, s.worker.cfg.consentTimeout)
	s.close()
}

// close ends the session: it sends close_notify to the caller (when DTLS is
// up) and removes the session from the worker. It is safe to call more than
// once and from any goroutine.
func (s *session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.consent.Stop()

		s.mu.Lock()
		dtlsConn := s.dtlsConn
		s.mu.Unlock()
		if dtlsConn != nil {
			_ = dtlsConn.Close()
		}
		_ = s.dtlsEndpoint.Close()

		s.worker.forget(s)
		s.log.Debugf("session %s: closed", s.id)
	})
}

func newTrackState(id string, media *offeredMedia) (trackState, error) {
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		return trackState{}, err
	}

	return trackState{
		ID:          id,
		MID:         media.mid,
		PayloadType: media.codec.payloadType,
		SSRC:        binary.BigEndian.Uint32(random[0:4]),
		InitialSeq:  binary.BigEndian.Uint16(random[4:6]),
		InitialTS:   binary.BigEndian.Uint32(random[6:10]),
	}, nil
}

func srtpProfile(profile dtls.SRTPProtectionProfile) (srtp.ProtectionProfile, error) {
	switch profile {
	case dtls.SRTP_AEAD_AES_128_GCM:
		return srtp.ProtectionProfileAeadAes128Gcm, nil
	case dtls.SRTP_AEAD_AES_256_GCM:
		return srtp.ProtectionProfileAeadAes256Gcm, nil
	case dtls.SRTP_AES128_CM_HMAC_SHA1_80:
		return srtp.ProtectionProfileAes128CmHmacSha1_80, nil
	case dtls.SRTP_AES128_CM_HMAC_SHA1_32:
		return srtp.ProtectionProfileAes128CmHmacSha1_32, nil
	default:
		return 0, errNoSRTPProfile
	}
}

// newCertificate generates a self-signed DTLS certificate and returns it as
// DER plus its private key as PKCS #8 DER.
func newCertificate() (certDER, keyDER []byte, err error) {
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, nil, fmt.Errorf("mediaworker: generate DTLS certificate: %w", err)
	}
	keyDER, err = x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("mediaworker: encode DTLS key: %w", err)
	}

	return cert.Certificate[0], keyDER, nil
}

// certificateFingerprint is the SDP sha-256 fingerprint of a DER certificate,
// in upper-case hex as RFC 8122 writes it.
func certificateFingerprint(certDER []byte) string {
	digest := sha256.Sum256(certDER)
	parts := make([]string, len(digest))
	for i, b := range digest {
		parts[i] = fmt.Sprintf("%02X", b)
	}

	return strings.Join(parts, ":")
}

const iceChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomString returns n random ICE characters (RFC 8445 ice-char, without
// '+' and '/' so the value is also URL-safe).
func randomString(n int) (string, error) {
	out := make([]byte, n)
	limit := big.NewInt(int64(len(iceChars)))
	for i := range out {
		v, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = iceChars[v.Int64()]
	}

	return string(out), nil
}
