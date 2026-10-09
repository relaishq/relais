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
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/fingerprint"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/sessionstore"
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
// atomic with the counters (see export).
type session struct {
	id     string // state.ID; never changes
	worker *Worker
	log    logging.LeveledLogger

	mu       sync.Mutex
	state    sessionState
	lease    sessionstore.Lease // guarded by mu; never part of the media snapshot
	dtlsConn *dtls.Conn
	srtpIn   *srtp.Context // decrypts caller to worker
	srtpOut  *srtp.Context // encrypts worker to caller

	// fenced is set under mu when exported, when its lease is lost, or when
	// a failed resume must close without disturbing the continuing caller.
	// From then on this worker processes none of the session's packets and
	// sends the caller nothing, not even the close_notify of closing the
	// DTLS connection: the session continues on another worker.
	fenced atomic.Bool

	// decryptFailures counts SRTP and SRTCP packets from the caller that this
	// worker could not decrypt.
	decryptFailures atomic.Uint64
	snapshotStored  atomic.Bool // internal harness readiness observation, no state bytes exposed

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
	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	snapshotMu     sync.Mutex // serialize encode/store without blocking packet processing
	snapshotWanted chan struct{}
	needsKeyframe  bool // resumed video may not yet have a known inbound SSRC
}

// newSession creates a session for an offer: the session state with fresh
// ICE credentials, a fresh DTLS certificate and an outbound echo track for
// each accepted m-line. The session answers the caller's ICE checks as soon
// as the worker registers it.
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
	audio, err := newTrackState(offer.accepted(mediaAudio))
	if err != nil {
		return nil, err
	}
	video, err := newTrackState(offer.accepted(mediaVideo))
	if err != nil {
		return nil, err
	}

	return sessionFromState(w, sessionState{
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
		Video: video,
	}), nil
}

// sessionFromState builds the runtime plumbing around a session state: a
// new session's, or one being resumed. The consent timer starts at once, and
// a shared socket routes the session to this worker.
func sessionFromState(w *Worker, state sessionState) *session {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		id:             state.ID,
		snapshotWanted: make(chan struct{}, 1),
		worker:         w,
		log:            w.cfg.LoggerFactory.NewLogger("session"),
		state:          state,
		nominated:      make(chan struct{}),
		rtcpBuf:        make([]byte, receiveMTU),
		decryptBuf:     make([]byte, receiveMTU),
		plainBuf:       make([]byte, receiveMTU),
		encryptBuf:     make([]byte, receiveMTU),
		ctx:            ctx,
		cancel:         cancel,
	}
	sess.dtlsEndpoint = newDTLSEndpoint(sess)
	sess.consent = time.AfterFunc(w.cfg.consentTimeout, sess.consentExpired)
	w.claimSession(sess.id)

	return sess
}

// answerParams is the session's half of the SDP answer.
func (s *session) answerParams() answerParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	tracks := make(map[string]trackState)
	for _, track := range []trackState{s.state.Audio, s.state.Video} {
		if track.negotiated() {
			tracks[track.MID] = track
		}
	}

	return answerParams{
		iceUfrag:    s.state.ICE.LocalUfrag,
		icePwd:      s.state.ICE.LocalPwd,
		fingerprint: certificateFingerprint(s.state.DTLS.Certificate),
		candidate:   s.worker.MediaAddr(),
		tracks:      tracks,
	}
}

// run drives the session after the answer is sent: it waits for the caller
// to nominate an address, runs the DTLS handshake as the DTLS server, derives
// the SRTP keys and keeps the session alive until the caller hangs up, its
// consent expires or the session is closed.
func (s *session) run() {
	defer s.close()

	dtlsConn, err := s.connect()
	if dtlsConn != nil {
		// close may already have run (and found no DTLS connection to
		// close), so run closes the connection it created too. A second
		// Close is harmless.
		defer func() { _ = dtlsConn.Close() }()
	}
	if err != nil {
		s.log.Warnf("session %s: %v", s.id, err)

		return
	}
	s.log.Infof("session %s: established with %s", s.id, s.dtlsEndpoint.RemoteAddr())
	s.serve(dtlsConn)
}

// connect waits for the caller's nomination, runs the DTLS handshake and
// starts SRTP. It returns the DTLS connection whenever it created one.
func (s *session) connect() (*dtls.Conn, error) {
	connectCtx, cancel := context.WithTimeout(s.ctx, s.worker.cfg.ConnectTimeout)
	defer cancel()

	select {
	case <-s.nominated:
	case <-connectCtx.Done():
		return nil, fmt.Errorf("caller did not complete ICE: %w", connectCtx.Err())
	}

	options, err := s.dtlsServerOptions()
	if err != nil {
		return nil, fmt.Errorf("DTLS options: %w", err)
	}
	dtlsConn, err := dtls.ServerWithOptions(s.dtlsEndpoint, s.dtlsEndpoint.RemoteAddr(), options...)
	if err != nil {
		return nil, fmt.Errorf("create DTLS server: %w", err)
	}
	s.mu.Lock()
	s.dtlsConn = dtlsConn
	s.mu.Unlock()

	if err := dtlsConn.HandshakeContext(connectCtx); err != nil {
		return dtlsConn, fmt.Errorf("DTLS handshake: %w", err)
	}
	if _, err := s.startSRTP(dtlsConn); err != nil {
		return dtlsConn, fmt.Errorf("start SRTP: %w", err)
	}

	if err := s.persistSnapshot(); err != nil {
		return dtlsConn, err
	}
	return dtlsConn, nil
}

// serve keeps an established session alive until the caller hangs up, its
// consent expires or the session is closed. WebRTC without data channels
// carries no DTLS application data, but the DTLS connection still has to be
// read so alerts are processed: a close_notify from the caller ends the
// session.
func (s *session) serve(dtlsConn *dtls.Conn) {
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
// DTLS connection, which is how a resumed session gets them. They are
// returned for that case.
func (s *session) startSRTP(conn *dtls.Conn) (srtp.SessionKeys, error) {
	dtlsProfile, ok := conn.SelectedSRTPProtectionProfile()
	if !ok {
		return srtp.SessionKeys{}, errNoSRTPProfile
	}
	profile, err := srtpProfile(dtlsProfile)
	if err != nil {
		return srtp.SessionKeys{}, err
	}

	connState, ok := conn.ConnectionState()
	if !ok {
		return srtp.SessionKeys{}, errors.New("mediaworker: DTLS connection state unavailable")
	}
	config := srtp.Config{Profile: profile}
	if err := config.ExtractSessionKeysFromDTLS(&connState, false); err != nil {
		return srtp.SessionKeys{}, err
	}

	in, err := srtp.CreateContext(config.Keys.RemoteMasterKey, config.Keys.RemoteMasterSalt, profile,
		srtp.SRTPReplayProtection(replayWindow), srtp.SRTCPReplayProtection(replayWindow))
	if err != nil {
		return srtp.SessionKeys{}, err
	}
	out, err := srtp.CreateContext(config.Keys.LocalMasterKey, config.Keys.LocalMasterSalt, profile)
	if err != nil {
		return srtp.SessionKeys{}, err
	}

	s.mu.Lock()
	s.state.SRTP.Profile = profile
	s.srtpIn = in
	s.srtpOut = out
	s.mu.Unlock()

	return config.Keys, nil
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
// outbound track for its payload type (Opus on the audio track, VP8 on the
// video track).
func (s *session) handleRTP(pkt []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srtpIn == nil || s.fenced.Load() {
		return // SRTP keys are not ready yet, or the session has moved.
	}

	// A successful decryption advances the inbound SRTP context's rollover
	// counter and replay window, so the session state records the packet's
	// index at once, from the header the context authenticated. Nothing that
	// can fail may sit between the two, or a snapshot would carry an index
	// behind the context's.
	var authenticated rtp.Header
	plain, err := s.srtpIn.DecryptRTP(s.decryptBuf, pkt, &authenticated)
	if err != nil {
		s.decryptFailures.Add(1)
		s.log.Debugf("session %s: drop SRTP packet: %v", s.id, err)

		return
	}
	oldInbound := s.state.SRTP.Inbound[authenticated.SSRC]
	s.state.SRTP.noteInbound(authenticated.SSRC, authenticated.SequenceNumber)
	if oldInbound>>16 != s.state.SRTP.Inbound[authenticated.SSRC]>>16 {
		s.wantSnapshot()
	}

	var in rtp.Packet
	if err := in.Unmarshal(plain); err != nil {
		s.log.Debugf("session %s: drop RTP packet: %v", s.id, err)

		return
	}

	track := s.trackFor(in.PayloadType)
	if track == nil {
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
	oldOutbound := track.HighestSentIndex
	track.noteSent(&header)
	if track.Packets == 1 {
		if roc, ok := s.srtpOut.ROC(track.SSRC); ok {
			track.HighestSentIndex = uint64(roc)<<16 | uint64(header.SequenceNumber)
		}
	}
	if track.Packets == 1 || oldOutbound>>16 != track.HighestSentIndex>>16 {
		s.wantSnapshot()
	}

	if _, err := s.worker.send(encrypted, s.state.ICE.RemoteAddr); err != nil {
		s.log.Debugf("session %s: send echo packet: %v", s.id, err)
	}
	if s.needsKeyframe && track == &s.state.Video {
		s.requestKeyframe("resume-first-video")
	}
	workerprobe.AfterEcho(s.worker.localAddr, s.ctx, s.id, s.plainBuf[:n])
}

// trackFor returns the outbound track that echoes a payload type, or nil.
func (s *session) trackFor(payloadType uint8) *trackState {
	for _, track := range []*trackState{&s.state.Audio, &s.state.Video} {
		if track.negotiated() && track.PayloadType == payloadType {
			return track
		}
	}

	return nil
}

// handleRTCP decrypts the caller's RTCP, which keeps the inbound SRTCP
// context current, and relays keyframe requests: when the caller's receiver
// asks for a keyframe (PLI or FIR) on the worker's video track, the worker
// asks the caller for a keyframe on the caller's source video. Other RTCP
// (reports, NACKs, ...) is not acted on.
func (s *session) handleRTCP(pkt []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srtpIn == nil || s.fenced.Load() {
		return
	}
	plain, err := s.srtpIn.DecryptRTCP(s.rtcpBuf, pkt, nil)
	if err != nil {
		s.decryptFailures.Add(1)
		s.log.Debugf("session %s: drop SRTCP packet: %v", s.id, err)

		return
	}
	if !s.state.Video.negotiated() {
		return
	}
	packets, err := rtcp.Unmarshal(plain)
	if err != nil {
		s.log.Debugf("session %s: drop RTCP packet: %v", s.id, err)

		return
	}
	if trigger := keyframeRequest(packets, s.state.Video.SSRC); trigger != "" {
		s.requestKeyframe(trigger)
	}
}

// keyframeRequest returns "PLI" or "FIR" when a compound RTCP packet asks
// for a keyframe on ssrc, and "" otherwise.
func keyframeRequest(packets []rtcp.Packet, ssrc uint32) string {
	for _, packet := range packets {
		switch p := packet.(type) {
		case *rtcp.PictureLossIndication:
			if p.MediaSSRC == ssrc {
				return "PLI"
			}
		case *rtcp.FullIntraRequest:
			for _, entry := range p.FIR {
				if entry.SSRC == ssrc {
					return "FIR"
				}
			}
		}
	}

	return ""
}

// requestKeyframe sends the caller a PLI for the video SSRC the video track
// echoes. It runs under mu. Until the first video packet arrives the worker
// does not know that SSRC, and there is no echo that could need a keyframe.
func (s *session) requestKeyframe(trigger string) {
	video := &s.state.Video
	if s.fenced.Load() || !video.Anchored {
		return
	}

	pli := rtcp.PictureLossIndication{SenderSSRC: video.SSRC, MediaSSRC: video.InboundSSRC}
	plain, err := pli.Marshal()
	if err != nil {
		s.log.Debugf("session %s: marshal PLI: %v", s.id, err)

		return
	}
	encrypted, err := s.srtpOut.EncryptRTCP(s.encryptBuf, plain, nil)
	if err != nil {
		s.log.Warnf("session %s: encrypt PLI: %v", s.id, err)

		return
	}
	s.encryptBuf = encrypted[:cap(encrypted)]
	if index, ok := s.srtpOut.Index(video.SSRC); ok {
		video.SRTCPIndex = index
	}

	if _, err := s.worker.send(encrypted, s.state.ICE.RemoteAddr); err != nil {
		s.log.Debugf("session %s: send PLI: %v", s.id, err)

		return
	}
	s.needsKeyframe = false
	s.log.Debugf("session %s: caller sent %s for video ssrc %d; sent PLI for caller ssrc %d",
		s.id, trigger, video.SSRC, video.InboundSSRC)
}

func (s *session) consentExpired() {
	s.log.Infof("session %s: no ICE consent check from the nominated address for %s, closing",
		s.id, s.worker.cfg.consentTimeout)
	s.close()
}

// close ends the session: it sends close_notify to the caller (when DTLS is
// up) and removes the session from the worker. A fenced session, one that
// was exported for a handover, sends nothing: it just leaves this worker. It
// is safe to call more than once and from any goroutine.
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
		s.worker.releaseSession(s.id)
		if s.fenced.Load() {
			s.log.Debugf("session %s: handed over", s.id)
		} else {
			s.log.Debugf("session %s: closed", s.id)
		}
	})
}

// newTrackState creates the outbound track that answers an accepted m-line.
// A nil m-line gives the zero value: a track that is not negotiated.
func newTrackState(media *offeredMedia) (trackState, error) {
	if media == nil {
		return trackState{}, nil
	}

	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		return trackState{}, err
	}

	return trackState{
		ID:          media.kind, // "audio" or "video"
		MID:         media.mid,
		PayloadType: media.codec.payloadType,
		SSRC:        binary.BigEndian.Uint32(random[0:4]),
		// Reserve the upper half for a first takeover margin. A caller that
		// has never received this SSRC starts at ROC 0, so even an unanchored
		// handshake snapshot must not make its first echo start at ROC 1.
		InitialSeq: binary.BigEndian.Uint16(random[4:6]) & 0x7fff,
		InitialTS:  binary.BigEndian.Uint32(random[6:10]),
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
