package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/sessionstore"
)

// A session moves between workers as bytes: ExportSession on the old owner,
// ResumeSession on the new one. Nothing else passes between them, so the
// same bytes can later go through a session store or across processes.
//
// What the bytes hold, and how the new owner uses it:
//
//   - ICE: the credentials and the nominated caller address. An ICE-lite
//     worker needs nothing else to answer the caller's consent checks.
//   - DTLS: the dtls.Conn's exported state (cipher suite, master secret,
//     epochs, record sequence number, the caller's certificate), resumed with
//     dtls.ResumeWithOptions. The SRTP keys are derived from it again, never
//     stored.
//   - SRTP, receive side: the highest index (rollover counter and sequence
//     number) per caller SSRC. Replay windows start fresh.
//   - SRTP, send side: per outbound track, the highest index sent and the
//     last SRTCP index, plus the rewriting offsets, so sequence numbers and
//     timestamps continue where the old owner stopped.

// ErrNotEstablished is returned when a session cannot be exported because
// its DTLS handshake has not completed.
var ErrNotEstablished = errors.New("mediaworker: session not established")

var (
	errSessionExists = errors.New("mediaworker: session already runs on this worker")
	errStateVersion  = errors.New("mediaworker: unsupported session state version")
	errBadState      = errors.New("mediaworker: invalid session state")
)

// resumeTimeout bounds rebuilding a resumed DTLS connection. It sends and
// waits for nothing, so it is quick; the bound only guards against a hang.
const resumeTimeout = 5 * time.Second

// ErrSequenceBudgetExhausted rejects retained RTP margins plus the caller's
// reserved outage gap and first resumed packet reaching the half sequence
// space. Unsent tracks must also retain a pre-wrap runway for lost starts.
var ErrSequenceBudgetExhausted = errors.New("mediaworker: sequence budget exhausted")

// ErrSRTCPIndexExhausted rejects index reuse under the same master key.
// RFC 3711 section 9.2 limits a master key to 2^31 SRTCP packets:
// https://www.rfc-editor.org/rfc/rfc3711.html#section-9.2
// Resume rejects a margin crossing that lifetime. Live sessions instead
// log encryption failure and stop sending PLIs until rekeying or termination.
var ErrSRTCPIndexExhausted = errors.New("mediaworker: SRTCP key lifetime exhausted")

// Keep at least 8193 ROC-zero packets before wrapping an unsent stream. A
// receiver losing its start needs an authenticated index above 2^15 before
// it can infer ROC 1; three default margins fit every production start.
const unsentRunway = 1 << 13

// SequenceGapReserve reserves 10,000 sequence numbers for the caller's own
// packets during an outage: a 2 s recovery target at up to 5,000 packets/s per
// outbound track. This budget is additional to retained takeover margins.
// Higher rates or longer outages require a larger reserve and fewer retries.
// The 8192 margin separately exceeds the conservative 5500 source indexes
// potentially used after a stale snapshot at 10,000 packets/s for 550 ms.
const SequenceGapReserve = 10000

// The next packet adds one index beyond the retained high water mark. Keep
// that packet plus the caller gap strictly below the half sequence space.
const maxRetainedSequenceAdvance = (1 << 15) - SequenceGapReserve - 2

// SequenceResumeAttempts derives the remaining safe margin applications from
// every negotiated track in the actual resumable state. The control plane
// caps its target retries by this minimum; ResumeSession rechecks the guard.
func SequenceResumeAttempts(data []byte, margin uint16) (int, error) {
	snap, err := decodeSnapshot(data)
	if err != nil {
		return 0, err
	}
	return snap.State.sequenceResumeAttempts(margin)
}

// SequenceResumeAttemptsWithReserve keeps the phase-1 minimum caller reserve
// and enlarges it for higher configured source rates during a two-second outage.
func SequenceResumeAttemptsWithReserve(data []byte, margin uint16, reserve uint32) (int, error) {
	snap, err := decodeSnapshot(data)
	if err != nil {
		return 0, err
	}
	return snap.State.sequenceResumeAttemptsWithReserve(margin, reserve)
}

func (state *sessionState) sequenceResumeAttempts(margin uint16) (int, error) {
	return state.sequenceResumeAttemptsWithReserve(margin, SequenceGapReserve)
}

func (state *sessionState) sequenceResumeAttemptsWithReserve(margin uint16, reserve uint32) (int, error) {
	reserve = max(reserve, SequenceGapReserve)
	if reserve >= 1<<15-1 {
		return 0, ErrSequenceBudgetExhausted
	}
	if margin == 0 {
		return 0, nil
	}
	attempts := 0xffff / int(margin)
	for _, track := range []*trackState{&state.Audio, &state.Video} {
		if !track.negotiated() {
			continue
		}
		if err := track.checkSequenceMarginWithReserve(margin, reserve); err != nil {
			return 0, err
		}
		remaining := (1 << 15) - int(reserve) - 2 - int(track.AdvanceSinceSend)
		if track.Packets == 0 {
			remaining = 0xffff - unsentRunway - int(track.InitialSeq)
		}
		attempts = min(attempts, remaining/int(margin))
	}
	if attempts == 0 {
		return 0, ErrSequenceBudgetExhausted
	}
	return attempts, nil
}

func (track *trackState) checkSequenceMarginWithReserve(margin uint16, reserve uint32) error {
	reserve = max(reserve, SequenceGapReserve)
	if !track.negotiated() || margin == 0 {
		return nil
	}
	if track.Packets == 0 {
		if uint64(track.InitialSeq)+uint64(margin) > 0xffff-unsentRunway {
			return ErrSequenceBudgetExhausted
		}
		return nil
	}
	if uint64(track.AdvanceSinceSend)+uint64(margin)+uint64(reserve)+1 >= 1<<15 {
		return ErrSequenceBudgetExhausted
	}
	return nil
}

// ResumeOptions shape how a resumed session continues outbound streams.
type ResumeOptions struct {
	Kind                 agent.ResumeKind
	InputMayBeDuplicated bool
	DuplicateWindows     []agent.DuplicateWindow
	// CallerSequenceReserve can enlarge, never reduce, the phase-1 reserve.
	CallerSequenceReserve uint32
	// Context optionally bounds rebuilding and persisting the resumed transport.
	// The adopted session has its own lifetime and outlives this context.
	Context context.Context
	// Lease is mandatory behind a relay: the control plane transfers it before resume.
	Lease sessionstore.Lease

	// SequenceMargin moves each outbound track's RTP sequence numbers (and
	// SRTP index) forward by this much, so no index the old owner may have
	// used after its snapshot is used again. Timestamps do not move. A
	// planned handover snapshots after the old owner stopped, so 0 is exact.
	// A snapshot that may be stale (a crash takeover) needs a margin larger
	// than the packets sent since it was taken. It must be below 2^15.
	SequenceMargin uint16

	// SRTCPIndexMargin does the same for the SRTCP index of the RTCP the
	// worker sends (its keyframe requests).
	SRTCPIndexMargin uint32
	// CheckpointAge is the store-clock age supplied at crash takeover.
	CheckpointAge      time.Duration
	SnapshotAge        time.Duration
	CheckpointStoredAt time.Time
}

// snapshot is the exported form of a session: the session state plus the
// established DTLS connection state, taken together under the session lock.
// It is encoded as JSON.
type snapshot struct {
	Version int // sessionStateVersion
	State   sessionState

	// DTLSConnection is pion/dtls State.MarshalBinary of the session's
	// dtls.Conn.
	DTLSConnection []byte
}

// ExportSession freezes a live session, snapshots it to bytes and drops it
// from this worker without telling the caller: the old owner's half of a
// planned handover (see Socket.Handover). From the moment of the snapshot
// the worker processes none of the session's packets and sends the caller
// nothing, so the snapshot is final and no packet is handled by two workers.
func (w *Worker) ExportSession(sessionID string) ([]byte, error) {
	sess := w.session(sessionID)
	if sess == nil {
		return nil, ErrUnknownSession
	}
	state, err := sess.export()
	if err != nil {
		return nil, err
	}
	sess.close() // fenced: its close_notify never leaves the worker

	return state, nil
}

// ResumeSession continues a session from bytes made by ExportSession, on
// this worker or another. It rebuilds the DTLS connection from the exported
// state, derives the SRTP keys again, restores the SRTP indexes and starts
// answering the caller's consent checks. The caller sees neither a new
// handshake nor a new nomination.
//
// On a shared Socket, Socket.Handover calls it and routes the caller's
// packets here. A worker with its own socket only receives the caller's
// packets if they reach that socket. Behind a relay, the control plane
// transfers ownership first and supplies the new fenced lease in opts.
func (w *Worker) ResumeSession(state []byte, opts ResumeOptions) (id string, resumeErr error) {
	defer func() {
		if resumeErr != nil {
			if opts.SequenceMargin > 0 {
				w.metrics.takeoverErrors.Add(1)
			} else {
				w.metrics.handoverErrors.Add(1)
			}
		}
	}()
	if opts.SequenceMargin >= 1<<15 {
		return "", fmt.Errorf("mediaworker: sequence margin %d is not below 2^15", opts.SequenceMargin)
	}
	parent := opts.Context
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return "", err
	}
	snap, err := decodeSnapshot(state)
	if err != nil {
		return "", err
	}
	// A retry after a lost reply must acknowledge the already adopted tenure,
	// without restoring counters or reapplying margins. Expiry time may have
	// advanced through renewal; identity is session, worker and epoch only.
	if existing := w.session(snap.State.ID); existing != nil {
		existing.mu.Lock()
		sameLease := opts.Lease.SessionID != "" && existing.lease.SessionID == opts.Lease.SessionID &&
			existing.lease.Worker == opts.Lease.Worker && existing.lease.Epoch == opts.Lease.Epoch && !existing.fenced.Load()
		existing.mu.Unlock()
		if sameLease {
			return snap.State.ID, nil
		}
		return "", errSessionExists
	}
	if opts.SequenceMargin > 0 {
		if _, err := snap.State.sequenceResumeAttemptsWithReserve(opts.SequenceMargin, opts.CallerSequenceReserve); err != nil {
			return "", err
		}
	}

	if w.cfg.Relay != nil {
		if opts.Lease.SessionID != snap.State.ID || opts.Lease.Worker != w.localAddr {
			return "", sessionstore.ErrLeaseLost
		}
		ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
		lease, err := w.cfg.Relay.Owners.Renew(ctx, opts.Lease, w.cfg.Relay.LeaseTTL)
		cancel()
		if err != nil {
			return "", err
		}
		opts.Lease = lease
	}
	sess := sessionFromState(w, snap.State)
	sess.lease = opts.Lease
	sess.callerSequenceReserve = opts.CallerSequenceReserve
	sess.state.Checkpoint.TakeoverAge = opts.CheckpointAge
	sess.state.Checkpoint.TakeoverSnapshotAge = opts.SnapshotAge
	sess.state.Checkpoint.TakeoverStoredAt = opts.CheckpointStoredAt
	if err := sess.initAgent(true, opts); err != nil {
		sess.fenced.Store(true)
		sess.close()
		// Restore failures are terminal. Remove this exact transferred lease
		// so existing control-plane rollback cannot resurrect the application.
		w.release(sess)
		return "", err
	}
	dtlsConn, err := sess.resume(snap.DTLSConnection, opts)
	if err != nil {
		sess.fenced.Store(true)
		sess.close()

		return "", fmt.Errorf("mediaworker: resume session %s: %w", sess.id, err)
	}
	if err := sess.persistSnapshotContext(parent); err != nil {
		sess.fenced.Store(true)
		sess.close()
		return "", err
	}
	if err := parent.Err(); err != nil {
		sess.fenced.Store(true)
		sess.close()
		return "", err
	}
	var replayPackets [][]byte
	if opts.SequenceMargin > 0 && w.cfg.socket == nil && sess.state.Video.negotiated() && w.cfg.cacheEnabled() {
		timeout := w.cfg.CacheReadTimeout
		if timeout <= 0 {
			timeout = 150 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		frames, cacheErr := w.cfg.FrameCache.Current(ctx, sess.id, framecache.Track{Kind: sess.state.Video.ID, SSRC: sess.state.Video.SSRC})
		cancel()
		if cacheErr != nil {
			reason := "cache-error"
			if errors.Is(cacheErr, context.DeadlineExceeded) {
				reason = "cache-timeout"
			}
			sess.skipReplay(reason)
			sess.log.Warnf("session %s: read cached group: %v", sess.id, cacheErr)
		} else {
			before := sess.state.Video
			if sess.reserveReplay(frames, opts.SequenceMargin) {
				if err := sess.persistSnapshotContext(parent); err != nil {
					// No replay ciphertext has been made or sent. Keep PLI recovery when
					// storage is unavailable, but a lost lease must still abort adoption.
					sess.state.Video = before
					sess.replay = nil
					sess.skipReplay("reservation-failure")
					if sess.fenced.Load() {
						return "", err
					}
					sess.log.Warnf("session %s: reserve cached replay: %v", sess.id, err)
				} else {
					replayPackets, err = sess.encodeReplay()
					if err != nil {
						sess.skipReplay("encoding-failure")
						sess.fenced.Store(true)
						sess.close()
						return "", err
					}
				}
			}
		}
	}

	if opts.SequenceMargin > 0 && sess.state.Video.negotiated() {
		if w.cfg.socket != nil {
			sess.skipReplay("shared-socket")
		} else if w.cfg.FrameCache == nil {
			sess.skipReplay("unconfigured")
		} else if !w.cfg.cacheEnabled() {
			sess.skipReplay("disabled")
		}
	}

	if err := parent.Err(); err != nil {
		sess.fenced.Store(true)
		sess.close()
		return "", err
	}
	sess.mu.Lock()
	sess.replaying = len(replayPackets) > 0
	if err := w.adopt(sess, dtlsConn, replayPackets); err != nil {
		sess.mu.Unlock()
		sess.fenced.Store(true)
		sess.close()

		return "", err
	}
	sess.needsKeyframe = opts.SequenceMargin > 0 && sess.state.Video.negotiated() && !w.cfg.DisableResumePLI
	// Shared-socket output is fenced until Handover switches the route. Keep
	// the request pending for the first routed video packet; skip replay there.
	if sess.needsKeyframe && w.cfg.socket == nil {
		sess.requestKeyframe("resume")
	}

	// A cached group can recover the source identity of an unanchored snapshot.
	if sess.needsKeyframe && w.cfg.socket == nil && sess.state.Video.Anchored {
		sess.requestKeyframe("resume-cache")
	}
	pendingPLI := sess.needsKeyframe
	sess.mu.Unlock()
	workerprobe.AfterResume(w.localAddr, sess.id, pendingPLI)
	sess.log.Infof("session %s: resumed with %s checkpoint_age=%s snapshot_age=%s", sess.id, snap.State.ICE.RemoteAddr, opts.CheckpointAge, opts.SnapshotAge)
	if opts.SequenceMargin > 0 {
		w.metrics.takeovers.Add(1)
	} else {
		w.metrics.handovers.Add(1)
	}

	return sess.id, nil
}

// SessionDecryptFailures returns how many of the caller's SRTP and SRTCP
// packets this worker could not decrypt in a session it runs. A resumed
// session counts from its resume.
func (w *Worker) SessionDecryptFailures(sessionID string) (uint64, error) {
	sess := w.session(sessionID)
	if sess == nil {
		return 0, ErrUnknownSession
	}

	return sess.decryptFailures.Load(), nil
}

// adopt registers a resumed session and serves it.
func (w *Worker) adopt(sess *session, dtlsConn *dtls.Conn, replayPackets [][]byte) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()

		return ErrClosed
	}
	if _, ok := w.sessions[sess.id]; ok {
		w.mu.Unlock()

		return errSessionExists
	}
	w.sessions[sess.id] = sess
	// The nominated address has passed ICE checks already: its DTLS and
	// SRTP belong to the session before the caller's next consent check.
	// No check has passed here yet, so the address does not stick to the
	// session on this worker until one does; the relay or shared socket
	// keeps the session's own check time across the move.
	w.byAddr[sess.state.ICE.RemoteAddr] = sess
	delete(w.byAddrConsent, sess.state.ICE.RemoteAddr)
	w.running.Add(3)
	if len(replayPackets) > 0 {
		w.running.Add(1)
	}
	w.mu.Unlock()
	if len(replayPackets) > 0 {
		go func() { defer w.running.Done(); sess.sendReplay(replayPackets) }()
	}
	go func() { defer w.running.Done(); sess.agentLoop() }()
	go func() { defer w.running.Done(); sess.snapshotLoop() }()

	go func() {
		defer w.running.Done()
		defer sess.close()
		sess.serve(dtlsConn)
	}()

	return nil
}

// export fences the session and snapshots it. The fence and the snapshot
// happen under mu, which every packet the session handles holds, so the
// counters in the snapshot are the final ones.
func (s *session) export() ([]byte, error) {
	// An export attempted before handshake must leave audio accepting input.
	s.mu.Lock()
	established := s.dtlsConn != nil && s.srtpIn != nil
	fenced := s.fenced.Load()
	s.mu.Unlock()
	if fenced {
		return nil, errHandedOver
	}
	if !established {
		return nil, ErrNotEstablished
	}
	if err := s.flushAgent(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if !s.fenced.Load() && s.ctx.Err() == nil && s.agent != nil {
			s.agent.paused = false
		}
	}()

	if s.fenced.Load() {
		return nil, errHandedOver
	}
	if s.dtlsConn == nil || s.srtpIn == nil {
		return nil, ErrNotEstablished
	}

	// Fence first, so the DTLS connection cannot send a record the snapshot
	// does not account for.
	s.fenced.Store(true)
	connState, ok := s.dtlsConn.ConnectionState()
	if !ok {
		s.fenced.Store(false)

		return nil, errors.New("mediaworker: DTLS connection state unavailable")
	}
	dtlsState, err := connState.MarshalBinary()
	if err != nil {
		s.fenced.Store(false)

		return nil, fmt.Errorf("mediaworker: export DTLS state: %w", err)
	}
	s.checkpointRates(&s.state.Checkpoint)
	state, err := json.Marshal(snapshot{
		Version:        sessionStateVersion,
		State:          s.state,
		DTLSConnection: dtlsState,
	})
	if err != nil {
		s.fenced.Store(false)

		return nil, fmt.Errorf("mediaworker: encode session state: %w", err)
	}

	return state, nil
}

func decodeSnapshot(data []byte) (*snapshot, error) {
	var version struct{ Version int }
	if err := json.Unmarshal(data, &version); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	if version.Version != sessionStateVersion && version.Version != 6 {
		return nil, fmt.Errorf("%w %d (want %d)", errStateVersion, version.Version, sessionStateVersion)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	state := &snap.State
	switch {
	case state.Version != version.Version:
		return nil, fmt.Errorf("%w %d (want %d)", errStateVersion, state.Version, sessionStateVersion)
	case state.ID == "" || state.ID != state.ICE.LocalUfrag:
		return nil, fmt.Errorf("%w: session ID %q, ICE ufrag %q", errBadState, state.ID, state.ICE.LocalUfrag)
	case !state.ICE.RemoteAddr.IsValid():
		return nil, fmt.Errorf("%w: no nominated caller address", errBadState)
	case len(snap.DTLSConnection) == 0 || state.SRTP.Profile == 0:
		return nil, fmt.Errorf("%w: no DTLS connection state", errBadState)
	}
	if version.Version == 6 {
		var legacy struct {
			State struct{ Agent json.RawMessage }
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("%w: %w", errBadState, err)
		}
		state.Agent.legacy = len(legacy.State.Agent) == 0
	}
	state.Version = sessionStateVersion
	snap.Version = sessionStateVersion
	if state.SRTP.Inbound == nil {
		state.SRTP.Inbound = make(map[uint32]uint64)
	}

	return &snap, nil
}

// resume rebuilds the session's transport from its exported DTLS state:
// the DTLS connection, SRTP keys and contexts, and the SRTP indexes. The
// session is not registered yet, so nothing else touches it.
func (s *session) resume(dtlsBytes []byte, opts ResumeOptions) (*dtls.Conn, error) {
	// The caller nominated its address before the snapshot.
	s.nominatedOnce.Do(func() { close(s.nominated) })

	var dtlsState dtls.State
	if err := dtlsState.UnmarshalBinary(dtlsBytes); err != nil {
		return nil, fmt.Errorf("DTLS state: %w", err)
	}
	// Role, cipher suite, epochs, master secret and SRTP profile all come
	// from the state; there is no handshake.
	dtlsConn, err := dtls.ResumeWithOptions(&dtlsState, s.dtlsEndpoint, s.dtlsEndpoint.RemoteAddr(),
		dtls.WithLoggerFactory(s.worker.cfg.LoggerFactory))
	if err != nil {
		return nil, fmt.Errorf("resume DTLS: %w", err)
	}
	s.mu.Lock()
	s.dtlsConn = dtlsConn // close closes it from here on
	exported := s.state.SRTP.Profile
	s.mu.Unlock()

	parent := opts.Context
	if parent == nil {
		parent = s.ctx
	}
	ctx, cancel := context.WithTimeout(parent, resumeTimeout)
	defer cancel()
	if err := dtlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("resume DTLS: %w", err)
	}

	keys, err := s.startSRTP(dtlsConn)
	if err != nil {
		return nil, fmt.Errorf("start SRTP: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SRTP.Profile != exported {
		return nil, fmt.Errorf("resumed DTLS state selects SRTP profile %d, session state says %d",
			s.state.SRTP.Profile, exported)
	}
	for ssrc, index := range s.state.SRTP.Inbound {
		if err := restoreInboundIndex(s.srtpIn, keys, s.state.SRTP.Profile, ssrc, index); err != nil {
			return nil, fmt.Errorf("restore inbound SRTP index for ssrc %d: %w", ssrc, err)
		}
	}
	for _, track := range []*trackState{&s.state.Audio, &s.state.Video} {
		if err := s.resumeTrack(track, opts); err != nil {
			return nil, fmt.Errorf("restore outbound %s track: %w", track.ID, err)
		}
	}

	return dtlsConn, nil
}

// resumeTrack continues an outbound track: its sequence numbers move forward
// by the margin (timestamps do not), and the outbound SRTP context continues
// from the highest index sent. It runs under mu.
func (s *session) resumeTrack(track *trackState, opts ResumeOptions) error {
	if !track.negotiated() {
		return nil
	}
	if err := track.checkSequenceMarginWithReserve(opts.SequenceMargin, opts.CallerSequenceReserve); err != nil {
		return err
	}
	// Pion SetIndex reduces modulo 2^31. A takeover margin must never
	// reset an exhausted SRTCP context and reuse ciphertext indexes.
	if uint64(track.SRTCPIndex)+uint64(opts.SRTCPIndexMargin) > 1<<31-1 {
		return ErrSRTCPIndexExhausted
	}
	// A later resume must also protect live indexes used since the burst.
	// Conservatively advance the persisted floor to the snapshot's high water
	// mark, including reserved but unsent indexes. In particular a margin-zero
	// handover cannot re-encrypt recently accepted source packets there.
	if track.ReplayFloor > 0 {
		track.ReplayFloor = track.HighestSentIndex
	}
	if track.Packets == 0 {
		// The handshake snapshot may predate the first media packet. Keep
		// ROC at zero: the receiver may never have seen this track.
		initial := uint64(track.InitialSeq) + uint64(opts.SequenceMargin)
		if opts.SequenceMargin > 0 && initial > 0xffff-unsentRunway {
			return ErrSequenceBudgetExhausted
		}
		track.AdvanceSinceSend += uint32(opts.SequenceMargin)
		track.InitialSeq += opts.SequenceMargin
		if opts.SequenceMargin > 0 {
			if err := restoreOutboundIndex(s.srtpOut, track.SSRC, initial-1); err != nil {
				return err
			}
		}
		track.SRTCPIndex += opts.SRTCPIndexMargin
		s.srtpOut.SetIndex(track.SSRC, track.SRTCPIndex)
		// Nothing sent: the first packet starts the stream, as it would have
		// on the old owner.
		return nil
	}

	track.AdvanceSinceSend += uint32(opts.SequenceMargin)
	margin := uint64(opts.SequenceMargin)
	track.SeqOffset += opts.SequenceMargin
	track.HighestSentIndex += margin
	if err := restoreOutboundIndex(s.srtpOut, track.SSRC, track.HighestSentIndex); err != nil {
		return err
	}

	if track.SRTCPIndex > 0 || opts.SRTCPIndexMargin > 0 {
		track.SRTCPIndex += opts.SRTCPIndexMargin
		s.srtpOut.SetIndex(track.SSRC, track.SRTCPIndex)
	}

	return nil
}

// pion/srtp cannot set an SSRC's full SRTP index (rollover counter and
// highest sequence number). SetROC sets only the rollover counter and makes
// the next packet's sequence number the highest, so a sequence-number wrap
// between the snapshot and the first resumed packet would break the stream
// for good. The two functions below set the full index instead by running
// one local "priming" packet at that index through the context, exactly as
// live traffic would. A primer never leaves the process. A proposed upstream
// patch (SRTPIndex/SetSRTPIndex, on the spike/session-resume branch under
// spikes/session-resume/upstream) would replace them.

// restoreInboundIndex makes an inbound context continue a caller's stream
// whose highest decrypted index is index. The primer is encrypted with the
// caller's keys by a throwaway context and decrypted by in; it also marks
// that index as seen in in's fresh replay window, which it was.
func restoreInboundIndex(in *srtp.Context, keys srtp.SessionKeys, profile srtp.ProtectionProfile,
	ssrc uint32, index uint64,
) error {
	primer, err := srtp.CreateContext(keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	if err != nil {
		return err
	}
	roc := uint32(index >> 16) //nolint:gosec // an SRTP index is 48 bits
	primer.SetROC(ssrc, roc)
	encrypted, err := primer.EncryptRTP(nil, primingPacket(ssrc, uint16(index)), nil) //nolint:gosec // low 16 bits
	if err != nil {
		return err
	}
	in.SetROC(ssrc, roc)
	if _, err := in.DecryptRTP(nil, encrypted, nil); err != nil {
		return err
	}

	return checkROC(in, ssrc, roc)
}

// restoreOutboundIndex makes an outbound context continue a stream whose
// highest sent index is index. The primer's ciphertext is discarded: it
// reuses the keystream of a packet already sent, so it must never be sent.
func restoreOutboundIndex(out *srtp.Context, ssrc uint32, index uint64) error {
	roc := uint32(index >> 16) //nolint:gosec // an SRTP index is 48 bits
	out.SetROC(ssrc, roc)
	if _, err := out.EncryptRTP(nil, primingPacket(ssrc, uint16(index)), nil); err != nil { //nolint:gosec // low 16 bits
		return err
	}

	return checkROC(out, ssrc, roc)
}

func checkROC(ctx *srtp.Context, ssrc, want uint32) error {
	if got, ok := ctx.ROC(ssrc); !ok || got != want {
		return fmt.Errorf("rollover counter %d after priming, want %d", got, want)
	}

	return nil
}

func primingPacket(ssrc uint32, seq uint16) []byte {
	packet := rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, SSRC: ssrc},
		Payload: []byte{0},
	}
	raw, _ := packet.Marshal() // a fixed header and a one-byte payload cannot fail

	return raw
}
