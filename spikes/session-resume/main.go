// Command session-resume is a throwaway spike for relaishq/relais#4.
//
// It answers: can a server-side WebRTC session built from Pion component
// libraries be exported mid-call to bytes and resumed by a freshly
// constructed endpoint on the same UDP socket, without a standard
// pion/webrtc v4 PeerConnection client noticing?
//
// Run from spikes/session-resume (its own Go module):
//
//	go run -race .                       (≈80 s: A->B at the caller's seq wrap, B->A' at 15 s, 60 s after)
//	go run -race . -close server -srtp-profile cm
//	go run -race . -margin 0 -rtcp-margin 0
//	go run -race . -lag 300ms            (stale snapshot, like a crash after a periodic snapshot)
//	go run -race . -lag 300ms -margin 0 -rtcp-margin 0   (negative control: seq reuse)
//	go run -race . -naive-roc            (negative control: SetROC-only inbound restore)
//	go run -race . -churn 30 -churn-every 500ms
//
// Each run ends with a REPORT block and RESULT: PASS|FAIL. Sample outputs are
// in results/. upstream/ holds a proposed pion/srtp patch (SetSRTPIndex).
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

var (
	flagMove1      = flag.Duration("move1", 5*time.Second, "fallback time (after media start) of move 1 A->B if the wrap trigger has not fired")
	flagMove2      = flag.Duration("move2", 15*time.Second, "time (after media start) of move 2 B->A")
	flagAfter      = flag.Duration("after", 60*time.Second, "keep the call up this long after the last move")
	flagPauseAt    = flag.Duration("pause-at", 30*time.Second, "after the last move, stop ALL server->client media/RTCP at this offset (STUN only)")
	flagPauseFor   = flag.Duration("pause-for", 12*time.Second, "length of the STUN-only window (client disconnected timeout is 5s)")
	flagMargin     = flag.Uint64("margin", 100, "outbound RTP sequence safety margin applied on resume")
	flagRTCPMargin = flag.Uint("rtcp-margin", 16, "outbound SRTCP index safety margin applied on resume")
	flagNaiveROC   = flag.Bool("naive-roc", false, "restore inbound SRTP with SetROC only (no highest-seq priming)")
	flagLag        = flag.Duration("lag", 0, "take the snapshot this long before the old endpoint dies (stale snapshot)")
	flagClose      = flag.String("close", "client", "who ends the call: client|server")
	flagInSeq      = flag.Int("in-seq-start", 65535-240, "client's first RTP seq (default wraps ~5s in)")
	flagOutSeq     = flag.Int("out-seq-start", 65536-300, "server's first echo RTP seq (default wraps across move 1 margin)")
	flagProfile    = flag.String("srtp-profile", "any", "SRTP profile the server accepts: any|gcm|cm")
	flagChurn      = flag.Int("churn", 0, "extra moves after move 2, one per -churn-every")
	flagChurnEvery = flag.Duration("churn-every", time.Second, "interval between churn moves")
	flagWrapMove   = flag.Bool("move-at-wrap", true, "fire move 1 right after the server sees inbound seq 65535")
)

var start = time.Now()

func logf(format string, args ...any) {
	fmt.Printf("%8.3fs "+format+"\n", append([]any{time.Since(start).Seconds()}, args...)...)
}

type pendingPkt struct {
	b    []byte
	from *net.UDPAddr
}

// Demux stands in for the relay: it owns the socket and hands each packet to
// whichever endpoint currently owns the session. While no endpoint owns it
// (during a move) packets are queued, like a new worker's socket buffer would.
type Demux struct {
	sock     *net.UDPConn
	mu       sync.Mutex
	cur      *Endpoint
	pending  []pendingPkt
	buffered atomic.Int64
	sniffed  atomic.Bool
}

func (d *Demux) set(ep *Endpoint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cur = ep
	if ep != nil {
		for _, p := range d.pending {
			ep.HandlePacket(p.b, p.from)
		}
		d.pending = nil
	}
}

func (d *Demux) run() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := d.sock.ReadFromUDP(buf)
		if err != nil {
			return
		}
		b := append([]byte{}, buf[:n]...)
		if classify(b) == "dtls" && !d.sniffed.Load() {
			if v, ok := sniffClientHello(b); ok {
				d.sniffed.Store(true)
				logf("DTLS ClientHello from client: %s", v)
			}
		}
		d.mu.Lock()
		if d.cur == nil {
			d.pending = append(d.pending, pendingPkt{b, addr})
			d.buffered.Add(1)
		} else {
			d.cur.HandlePacket(b, addr)
		}
		d.mu.Unlock()
	}
}

// sniffClientHello reports the record/client_version and whether the
// supported_versions extension (DTLS 1.3 offer) is present.
func sniffClientHello(b []byte) (string, bool) {
	if len(b) < 13+12+2 || b[0] != 22 || b[13] != 1 {
		return "", false
	}
	p := b[25:]
	cv := fmt.Sprintf("record=%x client_version=%x", b[1:3], p[:2])
	p = p[2+32:]
	skip := func(lenBytes int) bool {
		if len(p) < lenBytes {
			return false
		}
		l := 0
		for i := 0; i < lenBytes; i++ {
			l = l<<8 | int(p[i])
		}
		if len(p) < lenBytes+l {
			return false
		}
		p = p[lenBytes+l:]

		return true
	}
	if !skip(1) || !skip(1) || !skip(2) || !skip(1) || len(p) < 2 {
		return cv + " (no extensions)", true
	}
	p = p[2:]
	sv := "absent (DTLS 1.2 only)"
	for len(p) >= 4 {
		t := binary.BigEndian.Uint16(p)
		l := int(binary.BigEndian.Uint16(p[2:]))
		if len(p) < 4+l {
			break
		}
		if t == 0x002b {
			sv = fmt.Sprintf("present %x", p[4:4+l])
		}
		p = p[4+l:]
	}

	return cv + " supported_versions=" + sv, true
}

const iceChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = iceChars[rand.Intn(len(iceChars))]
	}

	return string(b)
}

func sdpAttr(sdp, name string) string {
	for _, l := range strings.Split(sdp, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "a="+name+":") {
			return strings.TrimPrefix(l, "a="+name+":")
		}
	}

	return ""
}

func buildAnswer(offer, ufrag, pwd, fp string, port int, ssrc uint32) string {
	mid := sdpAttr(offer, "mid")
	l := []string{
		"v=0", "o=- 4215775240449105457 2 IN IP4 127.0.0.1", "s=-", "t=0 0",
		"a=group:BUNDLE " + mid, "a=ice-lite", "a=msid-semantic: WMS relais",
		fmt.Sprintf("m=audio %d UDP/TLS/RTP/SAVPF 111", port), "c=IN IP4 127.0.0.1",
		"a=ice-ufrag:" + ufrag, "a=ice-pwd:" + pwd, "a=fingerprint:sha-256 " + fp, "a=setup:passive",
		"a=mid:" + mid, "a=sendrecv", "a=rtcp-mux", "a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;useinbandfec=1", "a=msid:relais echo",
		fmt.Sprintf("a=ssrc:%d cname:relais", ssrc), fmt.Sprintf("a=ssrc:%d msid:relais echo", ssrc),
		fmt.Sprintf("a=candidate:1 1 udp 2130706431 127.0.0.1 %d typ host", port), "a=end-of-candidates",
	}

	return strings.Join(l, "\r\n") + "\r\n"
}

type moveRec struct {
	label            string
	cutAt, resumedAt time.Time
	snapAt           time.Time
	blobLen, dtlsLen int
	stateJSON        string
	oldStats         string
	dtlsDesc         string
	oldFails         int64
}

func profiles(p string) []dtls.SRTPProtectionProfile {
	switch p {
	case "gcm":
		return []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM}
	case "cm":
		return []dtls.SRTPProtectionProfile{dtls.SRTP_AES128_CM_HMAC_SHA1_80}
	}

	return []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80}
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		logf("FATAL: %v", err)
		os.Exit(1)
	}
}

func run() error { //nolint:gocyclo
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	port := sock.LocalAddr().(*net.UDPAddr).Port
	demux := &Demux{sock: sock}
	go demux.run()

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	fpParts := make([]string, len(sum))
	for i, b := range sum {
		fpParts[i] = fmt.Sprintf("%02X", b)
	}

	client, err := NewClient(logf)
	if err != nil {
		return err
	}
	offer, err := client.Offer()
	if err != nil {
		return err
	}
	localUfrag, localPwd := randString(8), randString(24)
	outSSRC := rand.Uint32()
	answer := buildAnswer(offer, localUfrag, localPwd, strings.Join(fpParts, ":"), port, outSSRC)

	moveCh := make(chan struct{}, 1)
	var wrapArmed atomic.Bool
	cfg := EndpointConfig{
		Profiles: profiles(*flagProfile), OutSeqMargin: *flagMargin, RTCPIndexMargin: uint32(*flagRTCPMargin), NaiveROC: *flagNaiveROC, Logf: logf,
		OnInboundSeq: func(seq uint16) {
			if seq == 65535 && wrapArmed.CompareAndSwap(true, false) {
				select {
				case moveCh <- struct{}{}:
				default:
				}
			}
		},
	}
	A := NewFreshEndpoint("A", sock, &cert, localUfrag, localPwd, sdpAttr(offer, "ice-ufrag"), outSSRC, uint16(*flagOutSeq), cfg)
	demux.set(A)
	if err := client.Answer(answer); err != nil {
		return err
	}
	select {
	case <-client.connected:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("client never connected")
	}
	connectedAt := time.Now()
	logf("=== client connected; starting media")

	stop := make(chan struct{})
	go client.SendLoop(stop, uint16(*flagInSeq))
	go client.SampleConsent(stop)
	t0 := time.Now()
	wrapArmed.Store(*flagWrapMove)

	var moves []moveRec
	doMove := func(from *Endpoint, name string) (*Endpoint, error) {
		rec := moveRec{label: from.name + "->" + name}
		var blob []byte
		var err error
		if *flagLag > 0 {
			rec.snapAt = time.Now()
			blob, err = from.Export(false) // periodic-style snapshot, endpoint keeps running
			time.Sleep(*flagLag)
			rec.cutAt = time.Now()
			demux.set(nil)
		} else {
			rec.cutAt = time.Now()
			demux.set(nil) // relay stops delivering to the old owner
			rec.snapAt = time.Now()
			blob, err = from.Export(true)
		}
		from.Kill() // fenced: no close_notify reaches the client
		rec.oldStats = from.statsLine()
		rec.oldFails = from.Stats.SRTPFail.Load() + from.Stats.SRTCPFail.Load()
		if err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		to, err := ResumeEndpoint(name, sock, blob, cfg)
		if err != nil {
			return nil, fmt.Errorf("resume: %w", err)
		}
		demux.set(to)
		rec.resumedAt = time.Now()
		var s SessionState
		_ = json.Unmarshal(blob, &s)
		rec.blobLen, rec.dtlsLen = len(blob), len(s.DTLSState)
		rec.dtlsDesc = describeDTLSState(s.DTLSState)
		s.DTLSState = nil
		s.LocalPwd = "<redacted>"
		j, _ := json.Marshal(s)
		rec.stateJSON = string(j)
		moves = append(moves, rec)
		logf("=== MOVE %s: cut->resumed %v, state blob %d bytes (DTLS state %d bytes)",
			rec.label, rec.resumedAt.Sub(rec.cutAt).Round(time.Microsecond), rec.blobLen, rec.dtlsLen)

		return to, nil
	}

	// Move 1: A -> B (right after the caller's seq wraps 65535->0, or at the fallback time).
	select {
	case <-moveCh:
		logf("wrap trigger: server saw inbound seq 65535")
	case <-time.After(*flagMove1):
		logf("move-1 fallback timer fired")
	}
	cur, err := doMove(A, "B")
	if err != nil {
		return err
	}
	A = nil //nolint:ineffassign,wastedassign // old endpoint is gone; only bytes moved

	time.Sleep(time.Until(t0.Add(*flagMove2)))
	cur, err = doMove(cur, "A'")
	if err != nil {
		return err
	}
	for i := 0; i < *flagChurn; i++ {
		time.Sleep(*flagChurnEvery)
		name := "B"
		if i%2 == 1 {
			name = "A"
		}
		if cur, err = doMove(cur, fmt.Sprintf("%s%d", name, i+2)); err != nil {
			return err
		}
	}
	lastMove := time.Now()

	time.Sleep(time.Until(lastMove.Add(*flagPauseAt)))
	pauseStart := time.Now()
	cur.paused.Store(true)
	logf("=== PAUSE: server sends only STUN responses for %v", *flagPauseFor)
	time.Sleep(*flagPauseFor)
	cur.paused.Store(false)
	pauseEnd := time.Now()
	logf("=== PAUSE over: echo + SR resume")

	time.Sleep(time.Until(lastMove.Add(*flagAfter)))
	close(stop)
	endAt := time.Now()
	time.Sleep(100 * time.Millisecond)

	// End of call.
	var closeResult string
	switch *flagClose {
	case "server":
		_ = cur.CloseCall()
		select {
		case <-client.dtlsClosed:
			closeResult = "client DTLS transport closed after resumed server sent close_notify (server->client DTLS record decrypted OK)"
		case <-time.After(3 * time.Second):
			closeResult = "FAIL: client did not react to close_notify from resumed server within 3s"
		}
		_ = client.pc.Close()
	default:
		_ = client.pc.Close()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && cur.Stats.CloseNotifyAt.Load() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if cur.Stats.CloseNotifyAt.Load() != 0 {
			closeResult = "resumed server DTLS conn received + decrypted client close_notify (client->server DTLS record OK)"
		} else {
			closeResult = "FAIL: resumed server never saw client close_notify"
		}
	}
	cur.Kill()

	report(client, cur, moves, connectedAt, t0, lastMove, pauseStart, pauseEnd, endAt, closeResult, demux.buffered.Load())

	return nil
}
