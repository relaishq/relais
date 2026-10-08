package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/pion/stun/v4"
)

// SessionState is everything that moves between endpoints. It is plain data:
// the new endpoint gets these bytes and nothing else (plus the shared UDP
// socket, which in the real system is the relay).
type SessionState struct {
	// ICE (lite): only credentials and the selected remote address.
	LocalUfrag  string `json:"localUfrag"`
	LocalPwd    string `json:"localPwd"`
	RemoteUfrag string `json:"remoteUfrag"`
	RemoteAddr  string `json:"remoteAddr"`

	// DTLS: pion/dtls State.MarshalBinary (gob). SRTP keys are re-derived from it.
	DTLSState   []byte `json:"dtlsState"`
	SRTPProfile uint16 `json:"srtpProfile"`
	KeyCheck    string `json:"keyCheck"` // sha256 prefix of derived SRTP keys, to prove re-derivation matches

	Inbound     []InboundState `json:"inbound"`
	Outbound    OutboundState  `json:"outbound"`
	EchoSrcSSRC uint32         `json:"echoSrcSsrc"`
	ExportedAt  int64          `json:"exportedAt"`
}

// InboundState is the receive-side SRTP state per remote SSRC.
type InboundState struct {
	SSRC         uint32 `json:"ssrc"`
	HighestIndex uint64 `json:"highestIndex"` // ROC<<16 | highest seq (RFC 3711 packet index)
}

// OutboundState is the send-side state of our one outbound track.
type OutboundState struct {
	SSRC       uint32 `json:"ssrc"`
	PT         uint8  `json:"pt"`
	Started    bool   `json:"started"`
	LastExtSeq uint64 `json:"lastExtSeq"` // ROC<<16 | last seq sent
	TSOffset   uint32 `json:"tsOffset"`
	LastTS     uint32 `json:"lastTs"`
	LastSentAt int64  `json:"lastSentAt"`
	Packets    uint32 `json:"packets"`
	Octets     uint32 `json:"octets"`
	HasSRTCP   bool   `json:"hasSrtcp"`
	SRTCPIndex uint32 `json:"srtcpIndex"`
}

type EndpointConfig struct {
	Profiles        []dtls.SRTPProtectionProfile
	OutSeqMargin    uint64
	RTCPIndexMargin uint32
	NaiveROC        bool
	Logf            func(format string, args ...any)
	OnInboundSeq    func(seq uint16)
}

type inSSRC struct {
	highestIndex uint64
	seen         bool
}

type outTrack struct {
	ssrc       uint32
	pt         uint8
	started    bool
	extSeq     uint64
	tsOffset   uint32
	lastTS     uint32
	lastSentAt time.Time
	packets    uint32
	octets     uint32
}

type EndpointStats struct {
	StunReq, StunResp, StunBad     atomic.Int64
	RTPIn, RTPOut, RTCPIn, RTCPOut atomic.Int64
	SRTPFail, SRTCPFail            atomic.Int64
	NACKIn, RRIn, SRIn             atomic.Int64
	DroppedNotReady                atomic.Int64
	FirstRTPInAt, FirstRTPOutAt    atomic.Int64
	CloseNotifyAt                  atomic.Int64
	LastStunRespAt                 atomic.Int64
}

// Endpoint is a minimal WebRTC server endpoint: ICE-lite STUN responder,
// DTLS server, SRTP contexts, and an audio echo.
type Endpoint struct {
	name    string
	sock    *net.UDPConn
	cfg     EndpointConfig
	resumed bool

	fenced   atomic.Bool
	paused   atomic.Bool
	stopCh   chan struct{}
	stopOnce sync.Once
	Stats    EndpointStats

	mu          sync.Mutex
	frozen      bool
	localUfrag  string
	localPwd    string
	remoteUfrag string
	remoteAddr  *net.UDPAddr
	cert        *tls.Certificate

	pipe      *dtlsPipe
	dtlsConn  *dtls.Conn
	srtpReady bool
	profile   srtp.ProtectionProfile
	keys      srtp.SessionKeys
	rx, tx    *srtp.Context
	in        map[uint32]*inSSRC
	echoSrc   uint32
	out       outTrack
	firstRx   bool
}

func (e *Endpoint) logf(format string, args ...any) {
	e.cfg.Logf("[%s] "+format, append([]any{e.name}, args...)...)
}

// NewFreshEndpoint creates the endpoint that does the initial handshake.
func NewFreshEndpoint(name string, sock *net.UDPConn, cert *tls.Certificate,
	localUfrag, localPwd, remoteUfrag string, outSSRC uint32, outSeqStart uint16, cfg EndpointConfig,
) *Endpoint {
	e := &Endpoint{
		name: name, sock: sock, cfg: cfg, cert: cert, stopCh: make(chan struct{}),
		localUfrag: localUfrag, localPwd: localPwd, remoteUfrag: remoteUfrag,
		in: map[uint32]*inSSRC{},
	}
	if outSeqStart == 0 {
		outSeqStart = 1
	}
	// extSeq holds the last sent index; a fresh context starts in ROC 0.
	e.out = outTrack{ssrc: outSSRC, extSeq: uint64(outSeqStart) - 1}

	return e
}

func classify(b []byte) string {
	if len(b) == 0 {
		return "?"
	}
	switch c := b[0]; {
	case c < 4:
		return "stun"
	case c >= 20 && c <= 63:
		return "dtls"
	case c >= 128 && c <= 191:
		if len(b) >= 2 && b[1] >= 192 && b[1] <= 223 {
			return "rtcp"
		}

		return "rtp"
	}

	return "?"
}

func (e *Endpoint) HandlePacket(b []byte, from *net.UDPAddr) {
	switch classify(b) {
	case "stun":
		e.handleSTUN(b, from)
	case "dtls":
		e.handleDTLS(b, from)
	case "rtp":
		e.handleRTP(b)
	case "rtcp":
		e.handleRTCP(b)
	}
}

func (e *Endpoint) write(b []byte, to *net.UDPAddr) {
	if e.fenced.Load() || to == nil {
		return
	}
	_, _ = e.sock.WriteToUDP(b, to)
}

// ---- ICE-lite ----

func (e *Endpoint) handleSTUN(b []byte, from *net.UDPAddr) {
	m := &stun.Message{Raw: append([]byte{}, b...)}
	if err := m.Decode(); err != nil || m.Type != stun.BindingRequest {
		e.Stats.StunBad.Add(1)

		return
	}
	e.Stats.StunReq.Add(1)

	e.mu.Lock()
	ufrag, pwd, rufrag := e.localUfrag, e.localPwd, e.remoteUfrag
	frozen := e.frozen
	e.mu.Unlock()
	if frozen {
		return
	}

	var u stun.Username
	if err := u.GetFrom(m); err != nil || u.String() != ufrag+":"+rufrag {
		e.Stats.StunBad.Add(1)
		e.logf("STUN bad username %q", u.String())

		return
	}
	if err := stun.MessageIntegrity(pwd).Check(m); err != nil {
		e.Stats.StunBad.Add(1)
		e.logf("STUN bad integrity: %v", err)

		return
	}
	resp, err := stun.Build(
		stun.NewTransactionIDSetter(m.TransactionID),
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: from.IP, Port: from.Port},
		stun.NewShortTermIntegrity(pwd),
		stun.Fingerprint,
	)
	if err != nil {
		e.logf("STUN build: %v", err)

		return
	}
	e.mu.Lock()
	if e.remoteAddr == nil && m.Contains(stun.AttrUseCandidate) {
		e.remoteAddr = from
		e.logf("ICE nominated remote %s", from)
	}
	e.mu.Unlock()
	e.write(resp.Raw, from)
	e.Stats.StunResp.Add(1)
	e.Stats.LastStunRespAt.Store(time.Now().UnixNano())
}

// ---- DTLS ----

func (e *Endpoint) handleDTLS(b []byte, from *net.UDPAddr) {
	e.mu.Lock()
	if e.frozen {
		e.mu.Unlock()

		return
	}
	if e.pipe == nil {
		if e.resumed {
			e.mu.Unlock()

			return
		}
		if e.remoteAddr == nil {
			e.remoteAddr = from
		}
		e.pipe = newDTLSPipe(e.sock, from, &e.fenced)
		conn, err := dtls.Server(e.pipe, from, &dtls.Config{
			Certificates:           []tls.Certificate{*e.cert},
			SRTPProtectionProfiles: e.cfg.Profiles,
			ExtendedMasterSecret:   dtls.RequireExtendedMasterSecret,
			ClientAuth:             dtls.RequireAnyClientCert,
		})
		if err != nil {
			e.logf("dtls.Server: %v", err)
			e.mu.Unlock()

			return
		}
		e.dtlsConn = conn
		go e.runHandshake(conn)
	}
	pipe := e.pipe
	e.mu.Unlock()
	pipe.push(append([]byte{}, b...))
}

func (e *Endpoint) runHandshake(conn *dtls.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	if err := conn.HandshakeContext(ctx); err != nil {
		e.logf("DTLS handshake failed: %v", err)

		return
	}
	st, ok := conn.ConnectionState()
	if !ok {
		e.logf("no DTLS state")

		return
	}
	prof, _ := conn.SelectedSRTPProtectionProfile()
	e.mu.Lock()
	err := e.setupSRTP(&st, srtp.ProtectionProfile(prof))
	e.mu.Unlock()
	if err != nil {
		e.logf("SRTP setup: %v", err)

		return
	}
	e.logf("DTLS handshake done in %v: suite=%s srtp=%s", time.Since(t0).Round(time.Millisecond), st.CipherSuiteID, profName(srtp.ProtectionProfile(prof)))
	e.start()
}

func profName(p srtp.ProtectionProfile) string {
	switch p {
	case srtp.ProtectionProfileAeadAes128Gcm:
		return "SRTP_AEAD_AES_128_GCM"
	case srtp.ProtectionProfileAes128CmHmacSha1_80:
		return "SRTP_AES128_CM_HMAC_SHA1_80"
	}

	return fmt.Sprintf("0x%04x", uint16(p))
}

func (e *Endpoint) start() {
	go e.dtlsReadLoop(e.dtlsConn)
	go e.rtcpLoop()
}

func (e *Endpoint) dtlsReadLoop(conn *dtls.Conn) {
	buf := make([]byte, 8192)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if !e.fenced.Load() {
				e.Stats.CloseNotifyAt.Store(time.Now().UnixNano())
				e.logf("DTLS read ended (peer closed?): %v", err)
			}

			return
		}
		e.logf("DTLS app data %d bytes (unexpected, no data channel)", n)
	}
}

func keyCheck(k srtp.SessionKeys) string {
	h := sha256.New()
	h.Write(k.LocalMasterKey)
	h.Write(k.LocalMasterSalt)
	h.Write(k.RemoteMasterKey)
	h.Write(k.RemoteMasterSalt)

	return hex.EncodeToString(h.Sum(nil)[:8])
}

// setupSRTP derives SRTP keys from a DTLS State (live or unmarshaled) via the
// RFC 5764 exporter. Caller holds e.mu.
func (e *Endpoint) setupSRTP(exp srtp.KeyingMaterialExporter, prof srtp.ProtectionProfile) error {
	c := &srtp.Config{Profile: prof}
	if err := c.ExtractSessionKeysFromDTLS(exp, false); err != nil {
		return err
	}
	rx, err := srtp.CreateContext(c.Keys.RemoteMasterKey, c.Keys.RemoteMasterSalt, prof,
		srtp.SRTPReplayProtection(64), srtp.SRTCPReplayProtection(64))
	if err != nil {
		return err
	}
	tx, err := srtp.CreateContext(c.Keys.LocalMasterKey, c.Keys.LocalMasterSalt, prof)
	if err != nil {
		return err
	}
	e.profile, e.keys, e.rx, e.tx, e.srtpReady = prof, c.Keys, rx, tx, true

	return nil
}

// ---- SRTP media ----

func (e *Endpoint) handleRTP(b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.frozen || !e.srtpReady {
		e.Stats.DroppedNotReady.Add(1)

		return
	}
	dec, err := e.rx.DecryptRTP(nil, b, nil)
	if err != nil {
		e.Stats.SRTPFail.Add(1)
		var h rtp.Header
		_, _ = h.Unmarshal(b)
		e.logf("SRTP decrypt FAIL ssrc=%d seq=%d: %v", h.SSRC, h.SequenceNumber, err)

		return
	}
	var p rtp.Packet
	if err := p.Unmarshal(dec); err != nil {
		return
	}
	now := time.Now()
	e.Stats.RTPIn.Add(1)
	e.Stats.FirstRTPInAt.CompareAndSwap(0, now.UnixNano())

	st := e.in[p.SSRC]
	if st == nil {
		st = &inSSRC{}
		e.in[p.SSRC] = st
	}
	if !st.seen {
		st.highestIndex, st.seen = uint64(p.SequenceNumber), true
	} else if d := int16(p.SequenceNumber - uint16(st.highestIndex)); d > 0 {
		st.highestIndex += uint64(d)
	}
	if e.resumed && !e.firstRx {
		e.firstRx = true
		roc, _ := e.rx.ROC(p.SSRC)
		e.logf("first resumed inbound RTP decrypted: ssrc=%d seq=%d srtpROC=%d trackedIndex=%d", p.SSRC, p.SequenceNumber, roc, st.highestIndex)
	}
	if e.echoSrc == 0 {
		e.echoSrc = p.SSRC
	}
	if e.cfg.OnInboundSeq != nil {
		e.cfg.OnInboundSeq(p.SequenceNumber)
	}
	if p.SSRC == e.echoSrc && !e.paused.Load() {
		e.echo(&p, now)
	}
}

// echo sends the caller's packet back on our SSRC with our own seq space and a
// constant timestamp offset. Caller holds e.mu.
func (e *Endpoint) echo(in *rtp.Packet, now time.Time) {
	o := &e.out
	if !o.started {
		o.started, o.pt, o.tsOffset = true, in.PayloadType, rand.Uint32()
	}
	o.extSeq++
	p := rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: o.pt, SequenceNumber: uint16(o.extSeq),
			Timestamp: in.Timestamp + o.tsOffset, SSRC: o.ssrc, Marker: in.Marker,
		},
		Payload: in.Payload,
	}
	raw, err := p.Marshal()
	if err != nil {
		return
	}
	enc, err := e.tx.EncryptRTP(nil, raw, nil)
	if err != nil {
		e.logf("encrypt: %v", err)

		return
	}
	e.write(enc, e.remoteAddr)
	o.lastTS, o.lastSentAt = p.Timestamp, now
	o.packets++
	o.octets += uint32(len(p.Payload))
	e.Stats.RTPOut.Add(1)
	e.Stats.FirstRTPOutAt.CompareAndSwap(0, now.UnixNano())
}

func (e *Endpoint) handleRTCP(b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.frozen || !e.srtpReady {
		e.Stats.DroppedNotReady.Add(1)

		return
	}
	dec, err := e.rx.DecryptRTCP(nil, b, nil)
	if err != nil {
		e.Stats.SRTCPFail.Add(1)
		e.logf("SRTCP decrypt FAIL: %v", err)

		return
	}
	e.Stats.RTCPIn.Add(1)
	pkts, err := rtcp.Unmarshal(dec)
	if err != nil {
		return
	}
	for _, p := range pkts {
		switch p.(type) {
		case *rtcp.TransportLayerNack:
			e.Stats.NACKIn.Add(1)
		case *rtcp.ReceiverReport:
			e.Stats.RRIn.Add(1)
		case *rtcp.SenderReport:
			e.Stats.SRIn.Add(1)
		}
	}
}

func (e *Endpoint) rtcpLoop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			e.sendSR()
		}
	}
}

func ntp(t time.Time) uint64 {
	s := uint64(t.Unix()) + 2208988800
	f := uint64(t.Nanosecond()) * (1 << 32) / 1e9

	return s<<32 | f
}

func (e *Endpoint) sendSR() {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := &e.out
	if e.frozen || !e.srtpReady || !o.started || e.paused.Load() {
		return
	}
	now := time.Now()
	sr := rtcp.SenderReport{
		SSRC: o.ssrc, NTPTime: ntp(now),
		RTPTime:     o.lastTS + uint32(now.Sub(o.lastSentAt).Seconds()*48000),
		PacketCount: o.packets, OctetCount: o.octets,
	}
	raw, err := sr.Marshal()
	if err != nil {
		return
	}
	enc, err := e.tx.EncryptRTCP(nil, raw, nil)
	if err != nil {
		e.logf("encrypt rtcp: %v", err)

		return
	}
	e.write(enc, e.remoteAddr)
	e.Stats.RTCPOut.Add(1)
}

// ---- export / destroy / resume ----

// Export snapshots the session. With freeze=true (planned handover) the
// endpoint stops processing packets so no counter moves after the snapshot.
func (e *Endpoint) Export(freeze bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if freeze {
		e.frozen = true
	}
	if e.dtlsConn == nil || !e.srtpReady {
		return nil, errors.New("not established")
	}
	st, ok := e.dtlsConn.ConnectionState()
	if !ok {
		return nil, errors.New("no dtls state")
	}
	dtlsBytes, err := st.MarshalBinary()
	if err != nil {
		return nil, err
	}
	s := SessionState{
		LocalUfrag: e.localUfrag, LocalPwd: e.localPwd, RemoteUfrag: e.remoteUfrag,
		RemoteAddr: e.remoteAddr.String(), DTLSState: dtlsBytes, SRTPProfile: uint16(e.profile),
		KeyCheck: keyCheck(e.keys), EchoSrcSSRC: e.echoSrc, ExportedAt: time.Now().UnixNano(),
	}
	for ssrc, in := range e.in {
		if !in.seen {
			continue
		}
		if roc, ok := e.rx.ROC(ssrc); ok && roc != uint32(in.highestIndex>>16) {
			e.logf("WARN tracked inbound ROC %d != srtp ROC %d", in.highestIndex>>16, roc)
		}
		s.Inbound = append(s.Inbound, InboundState{SSRC: ssrc, HighestIndex: in.highestIndex})
	}
	o := e.out
	if roc, ok := e.tx.ROC(o.ssrc); ok && roc != uint32(o.extSeq>>16) {
		e.logf("WARN tracked outbound ROC %d != srtp ROC %d", o.extSeq>>16, roc)
	}
	s.Outbound = OutboundState{
		SSRC: o.ssrc, PT: o.pt, Started: o.started, LastExtSeq: o.extSeq, TSOffset: o.tsOffset,
		LastTS: o.lastTS, LastSentAt: o.lastSentAt.UnixNano(), Packets: o.packets, Octets: o.octets,
	}
	if idx, ok := e.tx.Index(o.ssrc); ok {
		s.Outbound.HasSRTCP, s.Outbound.SRTCPIndex = true, idx
	}

	return json.Marshal(s)
}

// Kill destroys the endpoint without telling the caller anything: writes are
// fenced first so DTLS close_notify (sent by dtls.Conn.Close) never leaves.
func (e *Endpoint) Kill() {
	e.fenced.Store(true)
	e.stopOnce.Do(func() { close(e.stopCh) })
	e.mu.Lock()
	e.frozen = true
	conn, pipe := e.dtlsConn, e.pipe
	e.dtlsConn, e.pipe, e.rx, e.tx, e.in = nil, nil, nil, nil, nil
	e.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	if pipe != nil {
		_ = pipe.Close()
	}
}

// CloseCall ends the call from the server side (sends DTLS close_notify).
func (e *Endpoint) CloseCall() error {
	e.mu.Lock()
	conn := e.dtlsConn
	e.mu.Unlock()
	e.stopOnce.Do(func() { close(e.stopCh) })

	return conn.Close()
}

// ResumeEndpoint builds a brand-new endpoint from exported bytes only.
func ResumeEndpoint(name string, sock *net.UDPConn, blob []byte, cfg EndpointConfig) (*Endpoint, error) {
	var s SessionState
	if err := json.Unmarshal(blob, &s); err != nil {
		return nil, err
	}
	raddr, err := net.ResolveUDPAddr("udp", s.RemoteAddr)
	if err != nil {
		return nil, err
	}
	e := &Endpoint{
		name: name, sock: sock, cfg: cfg, resumed: true, stopCh: make(chan struct{}),
		localUfrag: s.LocalUfrag, localPwd: s.LocalPwd, remoteUfrag: s.RemoteUfrag,
		remoteAddr: raddr, in: map[uint32]*inSSRC{}, echoSrc: s.EchoSrcSSRC,
	}

	// DTLS: unmarshal state and resume a dtls.Conn on a fresh pipe.
	var st dtls.State
	if err := st.UnmarshalBinary(s.DTLSState); err != nil {
		return nil, fmt.Errorf("dtls state: %w", err)
	}
	e.pipe = newDTLSPipe(sock, raddr, &e.fenced)
	// No options needed: role (IsClient), cipher suite, epochs, master secret
	// and SRTP profile all come from the state. Resume(state, conn, raddr,
	// &dtls.Config{}) (deprecated) behaves identically.
	conn, err := dtls.ResumeWithOptions(&st, e.pipe, raddr)
	if err != nil {
		return nil, fmt.Errorf("dtls resume: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("dtls resume handshake: %w", err)
	}
	e.dtlsConn = conn

	// SRTP: re-derive keys from the resumed DTLS state via the exporter.
	live, ok := conn.ConnectionState()
	if !ok {
		return nil, errors.New("resumed conn has no state")
	}
	if err := e.setupSRTP(&live, srtp.ProtectionProfile(s.SRTPProfile)); err != nil {
		return nil, err
	}
	if kc := keyCheck(e.keys); kc != s.KeyCheck {
		return nil, fmt.Errorf("re-derived SRTP keys differ: %s != %s", kc, s.KeyCheck)
	}

	// Inbound: restore highest index per SSRC.
	for _, in := range s.Inbound {
		if err := e.restoreInbound(in); err != nil {
			return nil, err
		}
	}

	// Outbound: jump seq forward by a margin, carry into ROC; jump SRTCP index.
	o := s.Outbound
	e.out = outTrack{
		ssrc: o.SSRC, pt: o.PT, started: o.Started, tsOffset: o.TSOffset, lastTS: o.LastTS,
		lastSentAt: time.Unix(0, o.LastSentAt), packets: o.Packets, octets: o.Octets, extSeq: o.LastExtSeq,
	}
	if o.Started {
		next := o.LastExtSeq + 1 + cfg.OutSeqMargin
		e.out.extSeq = next - 1
		e.tx.SetROC(o.SSRC, uint32(next>>16))
		e.logf("outbound: last seq=%d roc=%d -> next seq=%d roc=%d (margin %d)",
			uint16(o.LastExtSeq), o.LastExtSeq>>16, uint16(next), next>>16, cfg.OutSeqMargin)
	} else {
		e.tx.SetROC(o.SSRC, uint32((o.LastExtSeq+1)>>16))
	}
	if o.HasSRTCP {
		e.tx.SetIndex(o.SSRC, o.SRTCPIndex+cfg.RTCPIndexMargin)
		e.logf("outbound SRTCP index %d -> %d", o.SRTCPIndex, o.SRTCPIndex+cfg.RTCPIndexMargin)
	}
	e.start()

	return e, nil
}

// restoreInbound restores the receive-side packet index for one SSRC.
//
// pion/srtp only exposes SetROC(ssrc, roc), which sets index=roc<<16 and
// rolloverHasProcessed=false, so the next packet's seq is OR-ed in with that
// ROC no matter what. If the caller wrapped its 16-bit seq between the
// snapshot and the first packet the new endpoint sees, the ROC is wrong and
// every subsequent packet fails auth. Workaround: "prime" the context by
// decrypting one synthetic packet at the stored highest index, encrypted
// locally with the remote key. That sets index and rolloverHasProcessed=true
// exactly like live traffic would. The primer never leaves the process.
func (e *Endpoint) restoreInbound(in InboundState) error {
	roc, seq := uint32(in.HighestIndex>>16), uint16(in.HighestIndex)
	e.in[in.SSRC] = &inSSRC{highestIndex: in.HighestIndex, seen: true}
	e.rx.SetROC(in.SSRC, roc)
	if e.cfg.NaiveROC {
		e.logf("inbound ssrc=%d: NAIVE restore SetROC(%d) only (highest seq %d not restorable)", in.SSRC, roc, seq)

		return nil
	}
	primer, err := srtp.CreateContext(e.keys.RemoteMasterKey, e.keys.RemoteMasterSalt, e.profile)
	if err != nil {
		return err
	}
	primer.SetROC(in.SSRC, roc)
	p := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, SSRC: in.SSRC}, Payload: []byte{0}}
	raw, _ := p.Marshal()
	enc, err := primer.EncryptRTP(nil, raw, nil)
	if err != nil {
		return err
	}
	if _, err := e.rx.DecryptRTP(nil, enc, nil); err != nil {
		return fmt.Errorf("prime decrypt: %w", err)
	}
	got, _ := e.rx.ROC(in.SSRC)
	e.logf("inbound ssrc=%d: restored index roc=%d seq=%d via primer (srtp ROC now %d)", in.SSRC, roc, seq, got)

	return nil
}
