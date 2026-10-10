package mediaworker

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/pion/stun/v4"
)

// The media worker is an ICE-lite agent (RFC 8445 section 2.5): it never
// sends connectivity checks, it only answers the caller's. Answering a check
// needs nothing but the session's ICE credentials, so the worker answers
// them itself with pion/stun instead of running a pion/ice agent. That keeps
// the ICE state (ufrag, pwd and nominated address) in the session state, and
// a worker that resumes a session can answer the caller's consent checks
// without a new nomination.

const (
	// hostCandidatePriority is RFC 8445's recommended priority for a host
	// candidate: type preference 126, local preference 65535, component 1.
	hostCandidatePriority = 126<<24 | 65535<<8 | (256 - 1)

	// defaultConsentTimeout ends a session when the caller stops sending
	// authenticated connectivity checks from its nominated address (RFC 7675
	// consent freshness). Callers send one every few seconds.
	defaultConsentTimeout = 30 * time.Second
)

var (
	errActiveFlow = errors.New("mediaworker: caller address belongs to another active session, or the session has ended")
	errWrongUfrag = errors.New("mediaworker: STUN USERNAME does not match the caller's ICE ufrag")
	errHandedOver = errors.New("mediaworker: session has been handed over to another worker")
)

// hostCandidate is the a=candidate value for the worker's socket.
func hostCandidate(addr netip.AddrPort) string {
	return fmt.Sprintf("1 1 udp %d %s %d typ host", hostCandidatePriority, addr.Addr(), addr.Port())
}

// handleSTUN answers a STUN binding request that arrived on the worker's
// socket. The USERNAME (worker ufrag ":" caller ufrag) names the session,
// because the worker's ufrag is the session ID.
func (w *Worker) handleSTUN(raw []byte, from netip.AddrPort) {
	msg := &stun.Message{Raw: append([]byte(nil), raw...)}
	if err := msg.Decode(); err != nil || msg.Type != stun.BindingRequest {
		return
	}

	var username stun.Username
	if err := username.GetFrom(msg); err != nil {
		return
	}
	localUfrag, remoteUfrag, ok := strings.Cut(username.String(), ":")
	if !ok {
		return
	}

	sess := w.session(localUfrag)
	if sess == nil {
		return
	}
	if err := sess.answerBindingRequest(msg, remoteUfrag, from); err != nil {
		w.log.Debugf("session %s: drop binding request from %s: %v", localUfrag, from, err)
	}
}

// answerBindingRequest checks a binding request against the session's ICE
// credentials and sends the success response. A request with USE-CANDIDATE
// nominates the address it came from; a later one from another address
// re-nominates, and the worker moves the session there. Consent is per
// candidate pair (RFC 7675), so only valid requests from the nominated
// address refresh it. Valid requests from other addresses are answered but
// do not keep the session alive.
//
// Only a request that authenticates maps its address to the session, on
// the worker and on a shared socket, and only if no other session has
// authenticated a check from that address within the stickiness window.
// Otherwise the request is dropped unanswered and changes nothing. The
// response is sent under the session lock, like every other packet the
// session sends, so a session exported for a handover cannot answer after
// its snapshot.
//
// The caller is the controlling agent (the answer says a=ice-lite), so role
// attributes are not checked.
func (s *session) answerBindingRequest(msg *stun.Message, remoteUfrag string, from netip.AddrPort) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fenced.Load() {
		return errHandedOver
	}
	creds := &s.state.ICE
	if remoteUfrag != creds.RemoteUfrag {
		return errWrongUfrag
	}
	integrity := stun.NewShortTermIntegrity(creds.LocalPwd)
	if err := integrity.Check(msg); err != nil {
		return err
	}
	if err := stun.Fingerprint.Check(msg); err != nil {
		return err
	}

	// The caller proved it knows the session's ICE password from this
	// address, so its DTLS and SRTP from here belong to the session, unless
	// the address is still another session's.
	if !s.worker.mapAddr(from, s) {
		return errActiveFlow
	}

	if msg.Contains(stun.AttrUseCandidate) && creds.RemoteAddr != from {
		if creds.RemoteAddr.IsValid() {
			s.log.Debugf("session %s: caller re-nominated %s (was %s)", s.id, from, creds.RemoteAddr)
		} else {
			s.log.Debugf("session %s: caller nominated %s", s.id, from)
		}
		creds.RemoteAddr = from
		s.nominatedOnce.Do(func() { close(s.nominated) })
	}
	if from == creds.RemoteAddr {
		s.consent.Reset(s.worker.cfg.consentTimeout)
	}

	response, err := stun.Build(
		stun.NewTransactionIDSetter(msg.TransactionID),
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: from.Addr().AsSlice(), Port: int(from.Port())},
		integrity,
		stun.Fingerprint,
	)
	if err != nil {
		return err
	}

	if _, err := s.worker.conn.WriteToUDPAddrPort(response.Raw, from); err != nil {
		s.log.Debugf("session %s: send binding response: %v", s.id, err)
	}

	return nil
}
