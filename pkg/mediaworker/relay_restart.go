package mediaworker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
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
	// Required is the MID set control fixed for this attempt (ADR 0016
	// section 5). An older control omits it; the worker then fixes its own.
	Required []string `json:"required_mids,omitempty"`
}
type RelaySetRequest struct {
	Slots   []RelaySlot       `json:"slots"`
	Epochs  map[string]uint64 `json:"epochs"`
	Cutoffs map[string]uint64 `json:"cutoffs"`
}
type RestartReply struct {
	Answer   string   `json:"answer"`
	Required []string `json:"required_mids,omitempty"`
}
type relayPath struct{ leg, caller netip.AddrPort }
type pendingRestart struct {
	epoch    uint64 // relay enrollment that authorized this pending path
	ice      iceState
	receive  netip.AddrPort // authenticated pending receive path
	from     sessionstore.RelayAssignment
	answer   string
	required []string // fixed before the switch; C covers exactly these MIDs
	switched chan struct{}
	aborted  chan struct{}
}

// AbortRelayRestart revokes an unswitched generation, including its receive
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
	if s.revokedRelayLegs[path.leg] {
		p := s.pendingRestart
		if !pendingRTP || p == nil || p.ice.Relay.Leg != path.leg || p.receive != path.caller || !s.relayRestartAllowed(p) {
			return false
		}
	}
	if c, ok := s.worker.conn.(*relayConn); ok && !c.allowed(path.leg) {
		return false
	}
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
	if p != nil && s.revokedRelayLegs[path.leg] && !s.relayRestartAllowed(p) {
		return false
	}
	if p == nil || s.state.ICE.LocalUfrag == p.ice.LocalUfrag || path.leg != p.ice.Relay.Leg || path.caller != p.receive || !p.receive.IsValid() {
		return false
	}
	delete(s.revokedRelayLegs, path.leg)
	s.previousRelay = relayPath{s.currentLeg(), s.state.ICE.RemoteAddr}
	s.previousUntil = time.Now().Add(500 * time.Millisecond)
	p.ice.RemoteAddr = path.caller
	s.state.ICE = p.ice
	// Fix C's sequence boundaries before any media leaves on the new path.
	s.relayDelivery = s.newRelayDeliveryLocked(p.from.Generation+1, p.required)
	s.consent.Reset(s.worker.cfg.consentTimeout)
	if relaytrace.Enabled() {
		relaytrace.Emit("worker-switch", path.leg.String(), nil)
	}
	close(p.switched)
	return true
}

// ConfigureRelays adds acknowledged relay epochs and applies monotone cutoffs.
// Epochs belong to leg enrollment, not to the per-call media snapshot.
func (w *Worker) ConfigureRelays(ctx context.Context, req RelaySetRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, ok := w.conn.(*relayConn)
	if !ok || w.cfg.Relay.DialOut || w.cfg.SplitCheckpoints {
		return ErrRelayUnsupported
	}
	seen := map[string]bool{}
	for _, slot := range req.Slots {
		if slot.ID == "" || !slot.Leg.IsValid() || slot.Leg.Port() == 0 || !slot.Public.IsValid() || slot.Public.Port() == 0 || seen[slot.ID] || req.Epochs[slot.ID] == 0 {
			return ErrRelayUnsupported
		}
		seen[slot.ID] = true
	}
	if len(req.Epochs) != len(seen) {
		return ErrRelayUnsupported
	}
	for id, epoch := range req.Cutoffs {
		if id == "" || epoch == 0 {
			return ErrRelayUnsupported
		}
	}
	c.packetMu.Lock()
	defer c.packetMu.Unlock()
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	if w.relaySlots == nil && len(w.sessions) > 0 {
		w.mu.Unlock()
		return ErrRelayUnsupported
	}
	known := maps.Clone(w.relaySlots)
	if known == nil {
		known = map[string]RelaySlot{}
	}
	epochs := maps.Clone(w.relayEpochs)
	if epochs == nil {
		epochs = map[string]uint64{}
	}
	cutoffs := maps.Clone(w.relayCutoffs)
	if cutoffs == nil {
		cutoffs = map[string]uint64{}
	}
	var removed []netip.AddrPort
	for id, epoch := range req.Cutoffs {
		cutoffs[id] = max(cutoffs[id], epoch)
		if old, ok := known[id]; ok && epochs[id] <= epoch {
			removed = append(removed, old.Leg)
			delete(known, id)
			delete(epochs, id)
		}
	}
	for _, slot := range req.Slots {
		epoch := req.Epochs[slot.ID]
		old, enrolled := known[slot.ID]
		if epoch <= cutoffs[slot.ID] || epoch < epochs[slot.ID] || enrolled && epoch == epochs[slot.ID] && old != slot {
			w.mu.Unlock()
			return ErrRelayUnsupported
		}
		known[slot.ID], epochs[slot.ID] = slot, epoch
	}
	peers := make(map[netip.AddrPort]bool, len(known))
	for _, slot := range known {
		if peers[slot.Leg] {
			w.mu.Unlock()
			return ErrRelayUnsupported
		}
		peers[slot.Leg] = true
	}
	w.relaySlots, w.relayEpochs, w.relayCutoffs = known, epochs, cutoffs
	sessions := make([]*session, 0, len(w.sessions))
	for _, s := range w.sessions {
		sessions = append(sessions, s)
	}
	c.peersMu.Lock()
	c.peers = peers
	c.peersMu.Unlock()
	w.mu.Unlock()
	// Returning acknowledges cutoff, including in-flight admission and grace.
	for _, s := range sessions {
		s.mu.Lock()
		tracked := func(leg netip.AddrPort) bool {
			return leg == s.currentLeg() || leg == s.previousRelay.leg || s.pendingRestart != nil && leg == s.pendingRestart.ice.Relay.Leg
		}
		for leg := range s.revokedRelayLegs {
			if !tracked(leg) {
				delete(s.revokedRelayLegs, leg)
			}
		}
		for _, leg := range removed {
			if tracked(leg) {
				if s.revokedRelayLegs == nil {
					s.revokedRelayLegs = map[netip.AddrPort]bool{}
				}
				s.revokedRelayLegs[leg] = true
			}
		}
		kept := s.earlyPackets[:0]
		for _, packet := range s.earlyPackets {
			if !packet.path.leg.IsValid() || c.allowed(packet.path.leg) {
				kept = append(kept, packet)
			}
		}
		clear(s.earlyPackets[len(kept):])
		s.earlyPackets = kept
		s.mu.Unlock()
	}
	if relaytrace.Enabled() {
		for _, leg := range removed {
			relaytrace.Emit("worker-relay-cutoff", leg.String(), nil)
		}
	}
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

// The signature permits changed ICE credentials/candidates, nonzero m-line
// ports and port counts, session/media connection data, and a=rtcp default candidates.
// Those transport fields can change on ICE restart (RFC 8839 section 4.2.1.2).
// It also permits origin version changes and omitted extensions/feedback.
// SSRCs, MIDs, protocols, payload lists/order/mappings, BUNDLE, fingerprint
// and msid stay pinned. parseOffer separately validates the caller's setup
// role; the worker keeps its original passive answer role.
func restartIdentity(raw string) (string, error) {
	data, err := normalizeRestartOffer(raw)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// normalizeRestartOffer supplies the bytes for the persisted identity hash
// and the credential-free input to opt-in restart diagnostics.
func normalizeRestartOffer(raw string) ([]byte, error) {
	var d sdp.SessionDescription
	if err := d.UnmarshalString(raw); err != nil {
		return nil, ErrRestartMismatch
	}
	d.Origin.SessionVersion = 0
	d.ConnectionInformation = nil
	strip := func(attrs []sdp.Attribute) []sdp.Attribute {
		out := make([]sdp.Attribute, 0, len(attrs))
		for _, a := range attrs {
			switch a.Key {
			case "ice-ufrag", "ice-pwd", "candidate", "end-of-candidates", "rtcp", "setup", "extmap", "extmap-allow-mixed", "rtcp-fb":
			default:
				out = append(out, a)
			}
		}
		return out
	}
	d.Attributes = strip(d.Attributes)
	for _, m := range d.MediaDescriptions {
		// Port 0 rejects the m-section (RFC 3264 section 6); keep that pinned.
		if m.MediaName.Port.Value != 0 {
			m.MediaName.Port = sdp.RangedPort{Value: 9}
		}
		m.ConnectionInformation = nil
		m.Attributes = strip(m.Attributes)
	}
	return d.Marshal()
}

func (w *Worker) RestartSession(ctx context.Context, id string, req RestartRequest) (string, error) {
	reply, err := w.RestartRelaySession(ctx, id, req)
	return reply.Answer, err
}

// RestartRelaySession answers a restart offer and names the required MIDs the
// worker's C evidence covers for this generation.
func (w *Worker) RestartRelaySession(ctx context.Context, id string, req RestartRequest) (RestartReply, error) {
	if w.cfg.Relay == nil {
		return RestartReply{}, ErrRelayUnsupported
	}
	s := w.session(id)
	if s == nil {
		return RestartReply{}, ErrUnknownSession
	}
	owners, ok := w.cfg.Relay.Owners.(sessionstore.RelayAssignments)
	if !ok {
		return RestartReply{}, ErrRelayUnsupported
	}
	lease, a, err := owners.RelayOwner(ctx, id)
	if err != nil {
		return RestartReply{}, err
	}
	w.mu.Lock()
	slot, known := w.relaySlots[req.Slot]
	epoch := w.relayEpochs[req.Slot]
	w.mu.Unlock()
	if !known || a.Pending != req.Slot || req.Generation != a.Generation+1 {
		return RestartReply{}, sessionstore.ErrRelayConflict
	}
	identity, err := restartIdentity(req.Offer)
	if err != nil {
		traceRestartOffer("worker-restart-refused/identity_parse", id, "")
		return RestartReply{}, err
	}
	offer, err := parseOffer(req.Offer)
	if err != nil {
		return RestartReply{}, restartMismatch(id, "offer_parse", "")
	}
	params := s.answerParams()
	pwd, err := randomString(icePwdLength)
	if err != nil {
		return RestartReply{}, err
	}
	params.iceUfrag, params.icePwd, params.candidate = iceufrag.Format(id, req.Generation), pwd, slot.Public
	answer, err := buildAnswer(offer, params)
	if err != nil {
		return RestartReply{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Generation <= s.relayAborted {
		return RestartReply{}, sessionstore.ErrRelayConflict
	}
	if s.fenced.Load() || s.lease.Epoch != lease.Epoch || s.lease.Worker != lease.Worker {
		return RestartReply{}, sessionstore.ErrLeaseLost
	}
	switch {
	case s.componentMode:
		return RestartReply{}, restartMismatch(id, "component_mode", "")
	case s.state.Agent.V2 != nil:
		return RestartReply{}, restartMismatch(id, "agent_v2", "")
	case identity != s.state.RestartIdentity:
		return RestartReply{}, restartMismatch(id, "identity", req.Offer)
	case s.state.ICE.Relay == nil:
		return RestartReply{}, restartMismatch(id, "missing_relay", "")
	case s.state.ICE.Relay.ID != a.Slot:
		return RestartReply{}, restartMismatch(id, "relay_slot", "")
	}
	if s.pendingRestart != nil {
		if s.pendingRestart.from == a {
			if s.pendingRestart.ice.RemoteUfrag != offer.iceUfrag || s.pendingRestart.ice.RemotePwd != offer.icePwd {
				return RestartReply{}, restartMismatch(id, "pending_credentials", "")
			}
			return RestartReply{Answer: s.pendingRestart.answer, Required: slices.Clone(s.pendingRestart.required)}, nil
		}
		return RestartReply{}, sessionstore.ErrRelayConflict
	}
	required := s.requiredMIDsLocked()
	if req.Required != nil {
		for _, mid := range req.Required {
			if mid == "" || mid != s.state.Audio.MID && mid != s.state.Video.MID {
				return RestartReply{}, restartMismatch(id, "required_mid", "")
			}
		}
		required = slices.Clone(req.Required)
	}
	p := &pendingRestart{epoch: epoch, ice: iceState{LocalUfrag: params.iceUfrag, LocalPwd: pwd, RemoteUfrag: offer.iceUfrag, RemotePwd: offer.icePwd, Relay: &slot}, from: a, answer: answer, required: required, switched: make(chan struct{}), aborted: make(chan struct{})}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return RestartReply{}, ErrClosed
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
			err := owners.CommitRelay(commitCtx, lease, p.from, to)
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
	if relaytrace.Enabled() {
		relaytrace.Emit("worker-answer", slot.ID, nil)
	}
	return RestartReply{Answer: answer, Required: slices.Clone(p.required)}, nil
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
func (s *session) relayRestartAllowed(p *pendingRestart) bool {
	s.worker.mu.Lock()
	cutoff := s.worker.relayCutoffs[p.ice.Relay.ID]
	s.worker.mu.Unlock()
	return p.epoch > cutoff
}

func (s *session) send(pkt []byte, to netip.AddrPort) (int, error) {
	if s.revokedRelayLegs[s.currentLeg()] {
		return 0, ErrRelayUnsupported
	}
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
			if relaytrace.Enabled() {
				relaytrace.Emit("worker-leg-output", leg.String(), pkt)
			}
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
