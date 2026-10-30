package mediaworker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/relaytrace"

	"github.com/pion/sdp/v3"
	"github.com/relaishq/relais/internal/iceufrag"
	"github.com/relaishq/relais/pkg/sessionstore"
)

const relaySessionStateVersion = 11

var ErrRestartMismatch = errors.New("mediaworker: restart_mismatch")
var ErrRelaySwitched = errors.New("mediaworker: relay already switched")
var ErrRelayUnsupported = errors.New("mediaworker: relay change unsupported")

type RelaySlot struct {
	ID     string         `json:"id"`
	Leg    netip.AddrPort `json:"leg"`
	Public netip.AddrPort `json:"public"`
}
type RestartRequest struct {
	Offer      string `json:"offer"`
	Generation uint64 `json:"generation"`
	Slot       string `json:"slot"`
}
type RelaySetRequest struct {
	Slots []RelaySlot `json:"slots"`
}
type RestartReply struct {
	Answer string `json:"answer"`
}
type relayPath struct{ leg, caller netip.AddrPort }
type pendingRestart struct {
	ice      iceState
	receive  netip.AddrPort // authenticated pending receive path
	from     sessionstore.RelayAssignment
	answer   string
	switched chan struct{}
	aborted  chan struct{}
}

// AbortRelayRestart revokes an un-nominated generation, including its receive
// permission and credentials. Control calls it before the higher-generation
// assignment rollback. The current ICE credentials and output path stay live.
func (w *Worker) AbortRelayRestart(ctx context.Context, id string, generation uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := w.session(id)
	if s == nil {
		return ErrUnknownSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fenced.Load() {
		return sessionstore.ErrLeaseLost
	}
	p := s.pendingRestart
	_, current, _ := iceufrag.Parse(s.state.ICE.LocalUfrag)
	if current >= generation {
		return ErrRelaySwitched
	}
	if p == nil {
		s.relayAborted = max(s.relayAborted, generation)
		return nil
	}
	if generation != p.from.Generation+1 {
		return sessionstore.ErrRelayConflict
	}
	s.relayAborted = max(s.relayAborted, generation)
	s.pendingRestart = nil
	if s.previousRelay.leg == p.ice.Relay.Leg {
		s.previousRelay = relayPath{}
		s.previousUntil = time.Time{}
	}
	close(p.aborted)
	return nil
}

// acceptsRelayPath runs under the packet lock. Pending permission is RTP-only;
// it neither nominates the address nor refreshes the current pair's consent.
func (s *session) acceptsRelayPath(path relayPath, pendingRTP bool) bool {
	if path.leg == s.currentLeg() && (s.state.ICE.Relay == nil || path.caller == s.state.ICE.RemoteAddr) {
		return true
	}
	p := s.pendingRestart
	if p != nil && path.leg == p.ice.Relay.Leg {
		// Reusing a previous slot still needs this generation's check.
		return pendingRTP && p.receive.IsValid() && path.caller == p.receive
	}
	return path == s.previousRelay && time.Now().Before(s.previousUntil)
}

// switchRelayLocked follows the caller once, before emitting any media on the
// new path. Only authenticated checks or replay-checked SRTP call this method.
func (s *session) switchRelayLocked(path relayPath) bool {
	p := s.pendingRestart
	if p == nil || s.state.ICE.LocalUfrag == p.ice.LocalUfrag || path.leg != p.ice.Relay.Leg || path.caller != p.receive || !p.receive.IsValid() {
		return false
	}
	s.previousRelay = relayPath{s.currentLeg(), s.state.ICE.RemoteAddr}
	s.previousUntil = time.Now().Add(500 * time.Millisecond)
	p.ice.RemoteAddr = path.caller
	s.state.ICE = p.ice
	s.consent.Reset(s.worker.cfg.consentTimeout)
	if relaytrace.Enabled() {
		relaytrace.Emit("worker-switch", path.leg.String(), nil)
	}
	close(p.switched)
	return true
}

// ConfigureRelays is a private control operation, before admitting calls.
// Dial-out's per-slot observed-address registration is a separate contract.
func (w *Worker) ConfigureRelays(ctx context.Context, slots []RelaySlot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, ok := w.conn.(*relayConn)
	if !ok || w.cfg.Relay.DialOut || w.cfg.SplitCheckpoints || len(slots) < 2 {
		return ErrRelayUnsupported
	}
	known := make(map[string]RelaySlot, len(slots))
	peers := make(map[netip.AddrPort]bool, len(slots))
	bootstrap := false
	for _, slot := range slots {
		if slot.ID == "" || !slot.Leg.IsValid() || !slot.Public.IsValid() || known[slot.ID].ID != "" || peers[slot.Leg] {
			return ErrRelayUnsupported
		}
		known[slot.ID], peers[slot.Leg] = slot, true
		bootstrap = bootstrap || slot.Leg == c.relay
	}
	if !bootstrap {
		return ErrRelayUnsupported
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || len(w.sessions) > 0 {
		return ErrRelayUnsupported
	}
	w.relaySlots = known
	c.peersMu.Lock()
	c.peers = peers
	c.peersMu.Unlock()
	return nil
}

func (w *Worker) initialRelay(id string) *RelaySlot {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, slot := range w.relaySlots {
		if id != "" && slot.ID == id || id == "" && slot.Leg == w.cfg.Relay.Addr {
			copy := slot
			return &copy
		}
	}
	return nil
}

// The signature permits changed ICE credentials/candidates, origin version,
// and omitted unnegotiated extensions/feedback. It pins SSRCs, MIDs,
// payload mappings, BUNDLE and fingerprint. parseOffer separately validates
// the caller's setup role; the worker keeps its original passive answer role.
func restartIdentity(raw string) (string, error) {
	var d sdp.SessionDescription
	if err := d.UnmarshalString(raw); err != nil {
		return "", ErrRestartMismatch
	}
	d.Origin.SessionVersion = 0
	strip := func(attrs []sdp.Attribute) []sdp.Attribute {
		out := make([]sdp.Attribute, 0, len(attrs))
		for _, a := range attrs {
			switch a.Key {
			case "ice-ufrag", "ice-pwd", "candidate", "end-of-candidates", "setup", "extmap", "extmap-allow-mixed", "rtcp-fb":
			default:
				out = append(out, a)
			}
		}
		return out
	}
	d.Attributes = strip(d.Attributes)
	for _, m := range d.MediaDescriptions {
		m.Attributes = strip(m.Attributes)
	}
	data, err := d.Marshal()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (w *Worker) RestartSession(ctx context.Context, id string, req RestartRequest) (string, error) {
	if w.cfg.Relay == nil {
		return "", ErrRelayUnsupported
	}
	s := w.session(id)
	if s == nil {
		return "", ErrUnknownSession
	}
	owners, ok := w.cfg.Relay.Owners.(sessionstore.RelayAssignments)
	if !ok {
		return "", ErrRelayUnsupported
	}
	lease, a, err := owners.RelayOwner(ctx, id)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	slot, known := w.relaySlots[req.Slot]
	w.mu.Unlock()
	if !known || a.Pending != req.Slot || req.Generation != a.Generation+1 {
		return "", sessionstore.ErrRelayConflict
	}
	identity, err := restartIdentity(req.Offer)
	if err != nil {
		return "", err
	}
	offer, err := parseOffer(req.Offer)
	if err != nil {
		return "", ErrRestartMismatch
	}
	params := s.answerParams()
	pwd, err := randomString(icePwdLength)
	if err != nil {
		return "", err
	}
	params.iceUfrag, params.icePwd, params.candidate = iceufrag.Format(id, req.Generation), pwd, slot.Public
	answer, err := buildAnswer(offer, params)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Generation <= s.relayAborted {
		return "", sessionstore.ErrRelayConflict
	}
	if s.fenced.Load() || s.lease.Epoch != lease.Epoch || s.lease.Worker != lease.Worker {
		return "", sessionstore.ErrLeaseLost
	}
	if s.componentMode || s.state.Agent.V2 != nil || identity != s.state.RestartIdentity || s.state.ICE.Relay == nil || s.state.ICE.Relay.ID != a.Slot {
		return "", ErrRestartMismatch
	}
	if s.pendingRestart != nil {
		if s.pendingRestart.from == a {
			if s.pendingRestart.ice.RemoteUfrag != offer.iceUfrag || s.pendingRestart.ice.RemotePwd != offer.icePwd {
				return "", ErrRestartMismatch
			}
			return s.pendingRestart.answer, nil
		}
		return "", sessionstore.ErrRelayConflict
	}
	p := &pendingRestart{ice: iceState{LocalUfrag: params.iceUfrag, LocalPwd: pwd, RemoteUfrag: offer.iceUfrag, RemotePwd: offer.icePwd, Relay: &slot}, from: a, answer: answer, switched: make(chan struct{}), aborted: make(chan struct{})}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return "", ErrClosed
	}
	w.running.Add(1)
	w.mu.Unlock()
	s.pendingRestart = p
	go func() {
		defer w.running.Done()
		select {
		case <-p.switched:
		case <-p.aborted:
			return
		case <-s.ctx.Done():
			return
		}
		to := sessionstore.RelayAssignment{Slot: req.Slot, Generation: req.Generation}
		commitCtx, cancel := context.WithTimeout(s.ctx, sessionstore.StoreOutageBudget(w.cfg.Relay.StoreOutageBudget))
		defer cancel()
		for {
			// The packet switch already occurred. Store uncertainty never sends
			// media back to the old slot; readback settles an uncertain commit.
			err := owners.CASRelay(commitCtx, lease, p.from, to)
			if err != nil {
				_, got, e := owners.RelayOwner(commitCtx, id)
				if e == nil && got.Slot == to.Slot && got.Generation >= to.Generation {
					err = nil
				}
			}
			if err == nil {
				s.mu.Lock()
				if s.pendingRestart == p {
					s.pendingRestart = nil
				}
				s.mu.Unlock()
				return
			}
			if errors.Is(err, sessionstore.ErrLeaseLost) {
				s.close()
				return
			}
			select {
			case <-commitCtx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	relaytrace.Emit("worker-answer", slot.ID, nil)
	return answer, nil
}

func (s *session) currentLeg() netip.AddrPort {
	if s.state.ICE.Relay != nil {
		return s.state.ICE.Relay.Leg
	}
	if c, ok := s.worker.conn.(*relayConn); ok {
		return c.relay
	}
	return netip.AddrPort{}
}
func (s *session) send(pkt []byte, to netip.AddrPort) (int, error) {
	n, err := s.worker.sendVia(pkt, to, s.currentLeg())
	if err == nil && relaytrace.Enabled() {
		relaytrace.Emit("worker-output", s.currentLeg().String(), pkt)
	}
	return n, err
}
func (w *Worker) sendVia(pkt []byte, to, leg netip.AddrPort) (int, error) {
	if c, ok := w.conn.(*relayConn); ok && leg.IsValid() {
		n, err := c.writeVia(pkt, to, leg)
		if err == nil {
			w.metrics.packetsOut.Add(1)
			w.metrics.bytesOut.Add(uint64(n))
		}
		return n, err
	}
	return w.send(pkt, to)
}
func (w *Worker) receivedLeg() netip.AddrPort {
	if c, ok := w.conn.(*relayConn); ok {
		return c.lastRelay
	}
	return netip.AddrPort{}
}
func (w *Worker) sessionAtRelay(addr, leg netip.AddrPort) *session {
	if c, ok := w.conn.(*relayConn); !ok || leg == c.relay {
		return w.sessionAt(addr)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.byRelayPath[relayPath{leg, addr}]
}
func (w *Worker) mapRelayAddr(addr netip.AddrPort, s *session, leg netip.AddrPort) bool {
	if c, ok := w.conn.(*relayConn); !ok || leg == c.relay {
		return w.mapAddr(addr, s)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sessions[s.id] != s {
		return false
	}
	key := relayPath{leg, addr}
	if other := w.byRelayPath[key]; other != nil && other != s {
		return false
	}
	if w.byRelayPath == nil {
		w.byRelayPath = make(map[relayPath]*session)
	}
	w.byRelayPath[key] = s
	return true
}

func (w *Worker) CreateSessionAtRelay(ctx context.Context, offer, slot string) (id, answer string, lease sessionstore.Lease, err error) {
	id, answer, err = w.createSession(ctx, offer, slot, func(l sessionstore.Lease) { lease = l })
	return
}
