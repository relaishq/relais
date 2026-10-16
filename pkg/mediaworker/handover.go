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
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/sessionstore"
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
	// RelayReplay means the gated caller-media ring has complete codec continuity.
	// Keep the existing codec chain: no cached old pictures or proactive PLI.
	RelayReplay bool
	// RelayInputReplay means incomplete caller media is scheduled for delivery.
	// Complete rings use RelayReplay. Skip cached pictures and resume PLI;
	// the control plane requests a keyframe after an incomplete drain.
	// Empty rings leave this false and use normal frame-cache/PLI recovery.
	RelayInputReplay bool
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
	AgentMoveOnly  *movePayload `json:",omitempty"`
}

// ExportSession freezes a live session, snapshots it to bytes and drops it
// from this worker without telling the caller: the old owner's half of a
// planned handover (see Socket.Handover). From the moment of the snapshot
// the worker processes none of the session's packets and sends the caller
// nothing, so the snapshot is final and no packet is handled by two workers.
func (w *Worker) ExportSession(sessionID string) ([]byte, error) {
	state, release, err := w.exportSession(sessionID)
	defer release()
	// The returned bytes now belong to the caller, outside worker accounting.
	return state, err
}

// The private HTTP handler retains the export reservation until its response
// has been serialized and written, including concurrently held exports.
func (w *Worker) exportSession(sessionID string) ([]byte, func(), error) {
	sess := w.session(sessionID)
	if sess == nil {
		return nil, func() {}, ErrUnknownSession
	}
	state, release, err := sess.export()
	if err != nil {
		return nil, release, err
	}
	sess.close() // fenced: its close_notify never leaves the worker
	return state, release, nil
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

	var sess *session
	// V2 restore and Resumed.Save admit all move memory before extending
	// ownership. These local callbacks cannot emit output or launch effects.
	// Legacy retains its existing Renew-before-Restore ordering.
	if snap.State.Agent.V2 != nil {
		if w.cfg.Relay != nil {
			if opts.Lease.SessionID != snap.State.ID || opts.Lease.Worker != w.localAddr {
				return "", sessionstore.ErrLeaseLost
			}
			ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
			current, err := w.cfg.Relay.Owners.Get(ctx, snap.State.ID)
			cancel()
			if err != nil {
				return "", err
			}
			if current.Worker != opts.Lease.Worker || current.Epoch != opts.Lease.Epoch {
				return "", sessionstore.ErrLeaseLost
			}
		}
		sess = sessionFromState(w, snap.State)
		sess.lease = opts.Lease
		if err := sess.initAgent(true, opts, snap.AgentMoveOnly); err != nil {
			sess.fenced.Store(true)
			sess.close()
			return "", err
		}
	}
	if w.cfg.Relay != nil {
		if opts.Lease.SessionID != snap.State.ID || opts.Lease.Worker != w.localAddr {
			if sess != nil {
				sess.fenced.Store(true)
				sess.close()
			}
			return "", sessionstore.ErrLeaseLost
		}
		ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
		lease, err := w.cfg.Relay.Owners.Renew(ctx, opts.Lease, w.cfg.Relay.LeaseTTL)
		cancel()
		if err != nil {
			if sess != nil {
				sess.fenced.Store(true)
				sess.close()
			}
			return "", err
		}
		opts.Lease = lease
	}
	if sess == nil {
		sess = sessionFromState(w, snap.State)
	}
	sess.lease = opts.Lease
	sess.callerSequenceReserve = opts.CallerSequenceReserve
	sess.state.Checkpoint.TakeoverAge = opts.CheckpointAge
	sess.state.Checkpoint.TakeoverSnapshotAge = opts.SnapshotAge
	sess.state.Checkpoint.TakeoverStoredAt = opts.CheckpointStoredAt
	if sess.agent == nil {
		if err := sess.initAgent(true, opts, snap.AgentMoveOnly); err != nil {
			sess.fenced.Store(true)
			sess.close()
			// The control plane owns rollback/retry and terminal lease cleanup.
			return "", err
		}
	}
	dtlsConn, err := sess.resume(snap.DTLSConnection, opts)
	if err != nil {
		sess.fenced.Store(true)
		sess.close()

		return "", fmt.Errorf("mediaworker: resume session %s: %w", sess.id, err)
	}
	if err := sess.persistSnapshotMode(parent, true); err != nil {
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
	if opts.SequenceMargin > 0 && !opts.RelayReplay && !opts.RelayInputReplay && w.cfg.socket == nil && sess.state.Video.negotiated() && w.cfg.cacheEnabled() {
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
				if err := sess.persistSnapshotMode(parent, true); err != nil {
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
	sess.needsKeyframe = opts.SequenceMargin > 0 && !opts.RelayReplay && !opts.RelayInputReplay && sess.state.Video.negotiated() && !w.cfg.DisableResumePLI
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
// packets this worker could not authenticate or decrypt in a session it
// runs, excluding replay-window rejections. A resumed session counts from
// its resume.
func (w *Worker) SessionDecryptFailures(sessionID string) (uint64, error) {
	sess := w.session(sessionID)
	if sess == nil {
		return 0, ErrUnknownSession
	}

	return sess.decryptFailures.Load(), nil
}

// SessionReplayRejections returns how many caller SRTP and SRTCP packets the
// session's replay window rejected as duplicate or too old. These are not
// decryption failures; a resumed session counts from its resume.
func (w *Worker) SessionReplayRejections(sessionID string) (uint64, error) {
	sess := w.session(sessionID)
	if sess == nil {
		return 0, ErrUnknownSession
	}

	return sess.replayRejections.Load(), nil
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
func (s *session) export() (data []byte, release func(), exportErr error) {
	release = func() {}
	defer func() {
		if exportErr != nil {
			release()
		}
	}()
	// An export attempted before handshake must leave audio accepting input.
	s.mu.Lock()
	established := s.dtlsConn != nil && s.srtpIn != nil
	fenced := s.fenced.Load()
	s.mu.Unlock()
	if fenced {
		return nil, release, errHandedOver
	}
	if !established {
		return nil, release, ErrNotEstablished
	}
	var payload *movePayload
	var move chan agentMoveExport
	if s.agent != nil && s.agent.v2 != nil {
		move = make(chan agentMoveExport, 1)
	}
	var flushErr error
	if move != nil {
		flushErr = s.flushAgent(move)
	} else {
		flushErr = s.flushAgent()
	}
	if flushErr != nil {
		s.mu.Lock()
		if !s.fenced.Load() && s.ctx.Err() == nil && s.agent != nil {
			s.agent.paused = false
			if s.agent.v2 != nil {
				s.wakePacer()
			}
		}
		s.mu.Unlock()
		return nil, release, flushErr
	}
	if move != nil {
		export := <-move
		var err error
		release, err = s.claimV2ExportScratch()
		if err != nil {
			return nil, release, err
		}
		payload = &movePayload{Bytes: export.payload, Tasks: export.tasks, Audio: export.audio}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if !s.fenced.Load() && s.ctx.Err() == nil && s.agent != nil {
			s.agent.paused = false
			if s.agent.v2 != nil {
				s.wakePacer()
			}
		}
	}()

	if s.fenced.Load() {
		return nil, release, errHandedOver
	}
	if s.dtlsConn == nil || s.srtpIn == nil {
		return nil, release, ErrNotEstablished
	}

	// Fence first, so the DTLS connection cannot send a record the snapshot
	// does not account for.
	s.fenced.Store(true)
	connState, ok := s.dtlsConn.ConnectionState()
	if !ok {
		s.fenced.Store(false)

		return nil, release, errors.New("mediaworker: DTLS connection state unavailable")
	}
	dtlsState, err := connState.MarshalBinary()
	if err != nil {
		s.fenced.Store(false)

		return nil, release, fmt.Errorf("mediaworker: export DTLS state: %w", err)
	}
	s.checkpointRates(&s.state.Checkpoint)
	state, err := json.Marshal(snapshot{
		Version:        s.state.Version,
		AgentMoveOnly:  payload,
		State:          s.state,
		DTLSConnection: dtlsState,
	})
	if err != nil {
		s.fenced.Store(false)

		return nil, release, fmt.Errorf("mediaworker: encode session state: %w", err)
	}

	if payload != nil {
		if err := validateV2Wire(state); err != nil {
			s.fenced.Store(false)
			return nil, release, privateapi.Uncertain(err)
		}
	}
	return state, release, nil
}

func decodeSnapshot(data []byte) (*snapshot, error) {
	var version struct{ Version int }
	if err := json.Unmarshal(data, &version); err != nil {
		return nil, fmt.Errorf("%w: %w", errBadState, err)
	}
	if version.Version != v2SessionStateVersion && version.Version != sessionStateVersion && version.Version != 6 {
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
	if version.Version == v2SessionStateVersion {
		if state.Agent.V2 == nil || state.Agent.V2.Contract != v2Contract {
			return nil, fmt.Errorf("%w: missing v2 identity", errBadState)
		}
	} else {
		if state.Agent.V2 != nil || snap.AgentMoveOnly != nil {
			return nil, fmt.Errorf("%w: v2 data in legacy envelope", errBadState)
		}
		state.Version = sessionStateVersion
		snap.Version = sessionStateVersion
	}
	if snap.AgentMoveOnly != nil && len(snap.AgentMoveOnly.Bytes) > v2MaxWireBytes {
		return nil, fmt.Errorf("%w: oversized move payload", errBadState)
	}
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
	if err := s.restoreReplayInbound(keys, opts); err != nil {
		return nil, err
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
	if track.ReplayFloor > 0 && !s.exactPlannedVideo(track, opts) {
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

// exactPlannedVideo reports whether a positive ReplayFloor may stay where it
// is (ADR 0009). That needs a margin-zero resume of the video track whose
// inbound replay window was actually restored from an exact export: then no
// caller index any earlier owner accepted can be accepted, and so echoed, again.
// Mask presence or margin zero alone is not enough; every other resume
// advances the floor to the snapshot high water mark as before.
func (s *session) exactPlannedVideo(track *trackState, opts ResumeOptions) bool {
	return track == &s.state.Video && track.Anchored && opts.SequenceMargin == 0 &&
		!opts.InputMayBeDuplicated && s.restoredWindows[track.InboundSSRC]
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

// restoreInboundWindow restores an inbound context's replay window, not only
// its highest index: window bit i marks index high-i as already seen. One
// primer context encrypts every marked index in ascending order, ending at
// high. The receiver's ROC is set once, before the first primer, so later
// primers cross a sequence wrap by the ordinary RFC 3711 estimate.
func restoreInboundWindow(in *srtp.Context, keys srtp.SessionKeys, profile srtp.ProtectionProfile,
	ssrc uint32, high, window uint64,
) error {
	primer, err := srtp.CreateContext(keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	if err != nil {
		return err
	}
	first := true
	for offset := uint64(replayWindow - 1); ; offset-- {
		if offset <= high && (offset == 0 || window&(1<<offset) != 0) {
			index := high - offset
			roc := uint32(index >> 16) //nolint:gosec // an SRTP index is 48 bits
			primer.SetROC(ssrc, roc)
			encrypted, err := primer.EncryptRTP(nil, primingPacket(ssrc, uint16(index)), nil) //nolint:gosec // low 16 bits
			if err != nil {
				return err
			}
			if first {
				in.SetROC(ssrc, roc)
				first = false
			}
			if _, err := in.DecryptRTP(nil, encrypted, nil); err != nil {
				return err
			}
		}
		if offset == 0 {
			break
		}
	}

	return checkROC(in, ssrc, uint32(high>>16)) //nolint:gosec // an SRTP index is 48 bits
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

// Replay reopens only the audio input not represented by the restored agent
// pair. Outbound counters and persisted transport high water marks stay intact.
func (s *session) restoreReplayInbound(keys srtp.SessionKeys, opts ResumeOptions) error {
	floors := s.state.SRTP.Inbound
	if opts.InputMayBeDuplicated {
		var err error
		floors, err = replayInboundIndexes(s.state)
		if err != nil {
			return err
		}
	}
	for ssrc, high := range s.state.SRTP.Inbound {
		index, known := floors[ssrc]
		if !known {
			index = high
			for _, window := range opts.DuplicateWindows {
				if window.SSRC != ssrc {
					continue
				}
				if window.First >= 1<<48 || window.Last >= 1<<48 || window.Last < window.First {
					return fmt.Errorf("%w: invalid replay window", errBadState)
				}
				if window.First == 0 {
					// Do not mark the caller's first packet (index zero) as seen.
					s.srtpIn.SetROC(ssrc, 0)
					known = true
				} else {
					index = window.First - 1
				}
				break
			}
			if known {
				continue
			}
		}
		if opts.InputMayBeDuplicated && s.state.Agent.V2 != nil && s.state.Agent.Progress.Consumed > 0 &&
			ssrc == s.state.Agent.Progress.SSRC && index == s.state.Agent.Progress.Index {
			// V2 receipts may be pruned once received input passes their cause,
			// even while an input waits. Never reopen already consumed causes.
			if err := restoreInboundWindow(s.srtpIn, keys, s.state.SRTP.Profile, ssrc, index, ^uint64(0)); err != nil {
				return err
			}
			continue
		}
		window, exact := s.state.SRTP.Received[ssrc]
		if exact && index == high {
			// A margin-zero planned export is exact: restore its whole replay
			// window, so input any earlier owner consumed is rejected again.
			if err := restoreInboundWindow(s.srtpIn, keys, s.state.SRTP.Profile, ssrc, index, window); err != nil {
				return fmt.Errorf("restore inbound SRTP window for ssrc %d: %w", ssrc, err)
			}
			if s.restoredWindows == nil {
				s.restoredWindows = make(map[uint32]bool)
			}
			s.restoredWindows[ssrc] = true
			continue
		}
		if err := restoreInboundIndex(s.srtpIn, keys, s.state.SRTP.Profile, ssrc, index); err != nil {
			return fmt.Errorf("restore inbound SRTP index for ssrc %d: %w", ssrc, err)
		}
	}
	return nil
}
