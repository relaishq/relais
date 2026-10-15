package callharness

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

// plannedReorderCase delays one ciphertext datagram on the old worker's
// private leg until after the drain marker and the export (#127, ADR 0009).
type plannedReorderCase struct {
	name     string
	kind     string // kindAudio or kindVideo
	reorder  bool
	overtake int // later same-kind units that pass before the move (a gap)
	move     bool
	// Limits that keep #127's loss: an older relay without the ADR 0009
	// route, and a relay without the caller buffer.
	legacyRelay, bufferOff bool
}

func TestPlannedMoveReorderedPrivateInput(t *testing.T) {
	for _, tc := range []plannedReorderCase{
		{name: "ordered", kind: kindAudio, move: true},
		{name: "audio tail", kind: kindAudio, reorder: true, move: true},
		{name: "audio gap", kind: kindAudio, reorder: true, overtake: 2, move: true},
		{name: "video gap", kind: kindVideo, reorder: true, overtake: 1, move: true},
		{name: "delayed without a move", kind: kindAudio, reorder: true},
		{name: "older relay keeps the limit", kind: kindAudio, reorder: true, move: true, legacyRelay: true},
		{name: "buffer off keeps the limit", kind: kindAudio, reorder: true, move: true, bufferOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testPlannedMovePrivateInputOrder(t, tc) })
	}
}

// reorderUnits writes identified audio units or video frames by hand, so a
// test knows exactly which content the delayed datagram carried.
type reorderUnits struct {
	call  *Call
	audio *opusSource
	video *vp8Source
}

func newReorderUnits(t *testing.T, call *Call) *reorderUnits {
	t.Helper()
	audio, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	video, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	return &reorderUnits{call: call, audio: audio, video: video}
}

// send writes one unit and returns a function reporting whether it returned.
func (u *reorderUnits) send(t *testing.T, kind string) func() bool {
	t.Helper()
	rec := u.call.rec
	if kind == kindAudio {
		frame, duration, err := u.audio.next()
		require.NoError(t, err)
		frame, err = rec.sendingAudio(frame, duration)
		require.NoError(t, err)
		require.NoError(t, u.call.audio.WriteSample(media.Sample{Data: frame, Duration: duration}))
		rec.sent(kindAudio, false)
		return func() bool {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			return rec.sentAudioUnits[string(frame)].returnedAt != 0
		}
	}
	rec.mu.Lock()
	id := uint16(rec.sentVideo.Frames & 0x7fff) //nolint:gosec // the recorder's PictureID key
	rec.mu.Unlock()
	frame, keyframe, err := u.video.next()
	require.NoError(t, err)
	frame = rec.sendingVideo(frame, nil, u.video.frameDuration)
	require.NoError(t, u.call.video.WriteSample(media.Sample{Data: frame, Duration: u.video.frameDuration}))
	rec.sent(kindVideo, keyframe)
	return func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.sentVideoFrames[id].returnedAt != 0
	}
}

// settleLeg waits until no caller RTP has crossed the leg for quiet, so a
// frame still in flight from earlier media cannot be mistaken for the unit
// a test is about to send.
func settleLeg(t *testing.T, leg *privateLeg, quiet time.Duration) {
	t.Helper()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	leg.setIntercept(func(datagram []byte) bool {
		if _, payload, err := relay.ParseHeader(datagram); err == nil && len(payload) >= 12 && payload[0]>>6 == 2 {
			last.Store(time.Now().UnixNano())
		}
		return false
	})
	require.Eventually(t, func() bool { return time.Since(time.Unix(0, last.Load())) >= quiet }, 5*time.Second, time.Millisecond)
	leg.setIntercept(nil)
}

// sentSequence records the RTP sequence of each payload type's latest packet
// on the caller's own socket, before any network handling.
func sentSequence(call *Call) func(payloadType byte) uint16 {
	var mu sync.Mutex
	latest := make(map[byte]uint16)
	call.socket.observer.mu.Lock()
	call.socket.observer.inspectSend = func(raw []byte) {
		if len(raw) >= 12 && raw[0]>>6 == 2 && (raw[1] < 192 || raw[1] > 223) {
			mu.Lock()
			latest[raw[1]&0x7f] = uint16(raw[2])<<8 | uint16(raw[3])
			mu.Unlock()
		}
	}
	call.socket.observer.mu.Unlock()
	return func(payloadType byte) uint16 {
		mu.Lock()
		defer mu.Unlock()
		return latest[payloadType]
	}
}

// captureNext arms a leg to take the next caller RTP datagram of kind.
// Ordered capture only observes it and lets it pass.
func captureNext(leg *privateLeg, kind string, take bool) <-chan []byte {
	payloadType := byte(opusPayloadType)
	if kind == kindVideo {
		payloadType = vp8PayloadType
	}
	captured := make(chan []byte, 1)
	var armed atomic.Bool
	armed.Store(true)
	leg.setIntercept(func(datagram []byte) bool {
		_, payload, err := relay.ParseHeader(datagram)
		if err != nil || len(payload) < 12 || payload[0]>>6 != 2 || payload[1]&0x7f != payloadType || !armed.CompareAndSwap(true, false) {
			return false
		}
		captured <- datagram
		return take
	})
	return captured
}

func capturedHeader(t *testing.T, datagram []byte) rtp.Header {
	t.Helper()
	_, payload, err := relay.ParseHeader(datagram)
	require.NoError(t, err)
	var header rtp.Header
	_, err = header.Unmarshal(payload)
	require.NoError(t, err)
	return header
}

// sourceHigh reads the old worker's authenticated high water mark for ssrc.
func sourceHigh(t *testing.T, w *mediaworker.Worker, id string, ssrc uint32) (uint16, bool) {
	t.Helper()
	state, err := w.SnapshotSession(id)
	if err != nil {
		return 0, false
	}
	inbound, err := mediaworker.SnapshotInboundIndexes(state)
	require.NoError(t, err)
	high, ok := inbound[ssrc]
	return uint16(high), ok //nolint:gosec // low 16 bits
}

// decryptCounts counts authenticated caller indexes across every worker.
type decryptCounts struct {
	mu     sync.Mutex
	counts map[plannedPacketKey]int
}

func countDecrypts(t *testing.T, workers []*mediaworker.Worker) *decryptCounts {
	t.Helper()
	d := &decryptCounts{counts: make(map[plannedPacketKey]int)}
	d.observe(t, workers)
	return d
}

func (d *decryptCounts) observe(t *testing.T, workers []*mediaworker.Worker) {
	t.Helper()
	for _, w := range workers {
		require.NoError(t, workerprobe.SetAfterDecrypt(w.LocalAddr(), func(_ string, ssrc uint32, index uint64) {
			d.mu.Lock()
			d.counts[plannedPacketKey{ssrc: ssrc, seq: uint16(index)}]++ //nolint:gosec // low 16 bits
			d.mu.Unlock()
		}))
	}
}

func (d *decryptCounts) requireOnce(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, count := range d.counts {
		require.Equal(t, 1, count, "caller SSRC %d seq %d authenticated once across workers", key.ssrc, key.seq)
	}
}

func (d *decryptCounts) get(header rtp.Header) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[plannedPacketKey{ssrc: header.SSRC, seq: header.SequenceNumber}]
}

// requireNoDuplicates checks the caller saw every echoed packet and unit once.
func requireNoDuplicates(t *testing.T, call *Call, report *Report) {
	t.Helper()
	for _, kind := range []string{kindAudio, kindVideo} {
		if track := report.Track(kind); track != nil {
			require.Zero(t, track.DuplicatePackets, "%s duplicate packets", kind)
		}
	}
	call.rec.mu.Lock()
	defer call.rec.mu.Unlock()
	require.Empty(t, call.rec.repeatedAudioReturns, "audio units returned twice")
}

func testPlannedMovePrivateInputOrder(t *testing.T, tc plannedReorderCase) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := reorderOptions{buffer: !tc.bufferOff}
	if tc.legacyRelay {
		opts.relayHandler = legacyRelayHandler
	}
	sys := startReorderTopology(t, opts)
	decrypts := countDecrypts(t, sys.workers)
	call, err := sys.h.Dial(ctx, CallOptions{Video: true, Worker: 0})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
	settleLeg(t, sys.legs[0], 100*time.Millisecond)
	units := newReorderUnits(t, call)
	sent := sentSequence(call)
	captured := captureNext(sys.legs[0], tc.kind, tc.reorder)
	payloadType := byte(opusPayloadType)
	if tc.kind == kindVideo {
		payloadType = vp8PayloadType
	}
	returned := units.send(t, tc.kind)
	unitSeq := sent(payloadType)
	var late []byte
	select {
	case late = <-captured:
	case <-ctx.Done():
		t.Fatal("proxy did not capture input")
	}
	sys.legs[0].setIntercept(nil)
	header := capturedHeader(t, late)
	// The proxy delayed the unit under test: the audio packet, or the first
	// packet of the video frame.
	require.GreaterOrEqual(t, int16(unitSeq-header.SequenceNumber), int16(0)) //nolint:gosec // serial distance
	if tc.kind == kindAudio {
		require.Equal(t, unitSeq, header.SequenceNumber)
	}
	for range tc.overtake {
		units.send(t, tc.kind)
	}
	// The old worker authenticated everything before the marker except the
	// delayed datagram: a tail (high = seq-1) or a gap below its high mark.
	require.Eventually(t, func() bool {
		high, ok := sourceHigh(t, sys.workers[0], call.SessionID(), header.SSRC)
		distance := int16(high - header.SequenceNumber) //nolint:gosec // serial distance
		switch {
		case !ok:
			return false
		case !tc.reorder:
			return distance >= 0
		case tc.overtake > 0 || tc.kind == kindVideo:
			return distance > 0
		default:
			return distance == -1
		}
	}, 2*time.Second, time.Millisecond)

	var result controlplane.MoveResult
	if tc.move {
		result, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		call.RecordExternalMove(result)
		require.Zero(t, sys.workers[0].SessionCount())
		require.Equal(t, 1, sys.workers[1].SessionCount())
	}
	if tc.reorder {
		// After the export fence (or, without a move, merely late).
		sys.legs[0].deliver(late)
	}
	require.NoError(t, call.SendMedia(ctx, 400*time.Millisecond))
	time.Sleep(50 * time.Millisecond)

	limit := tc.legacyRelay || tc.bufferOff
	wantLost, wantRecovered := 0, 0
	if tc.reorder && tc.move {
		if limit {
			wantLost = 1
		} else {
			wantRecovered = 1
		}
	}
	active := sys.workers[0]
	if tc.move {
		active = sys.workers[1]
	}
	failures, err := active.SessionDecryptFailures(call.SessionID())
	require.NoError(t, err)
	replays, err := active.SessionReplayRejections(call.SessionID())
	require.NoError(t, err)
	stats := sys.relay.Stats()
	t.Logf("PLANNED_REORDER case=%q returned=%t recovered=%d held=%d lock_held=%s decrypt_failures=%d replay_rejections=%d delayed_seq=%d duration=%s",
		tc.name, returned(), result.Result.RecoveredPackets, result.Result.HeldPackets, result.Result.ReleaseLockHeld, failures, replays, header.SequenceNumber, result.Result.Duration)
	require.Equal(t, wantLost == 0, returned(), "delayed unit returned")
	require.Equal(t, wantRecovered, result.Result.RecoveredPackets)
	require.Zero(t, failures)
	require.Zero(t, replays)
	require.Zero(t, stats.WorkerNoFlow)
	require.Zero(t, stats.HoldDrops)
	require.Zero(t, stats.HoldSendFailures)
	if wantLost == 0 {
		require.Equal(t, 1, decrypts.get(header), "the delayed datagram is authenticated exactly once")
	} else {
		require.Zero(t, decrypts.get(header))
	}
	decrypts.requireOnce(t)

	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	requireNoDuplicates(t, call, report)
	if !tc.move {
		require.Empty(t, report.Moves)
		return
	}
	require.Len(t, report.Moves, 1)
	loss := report.Moves[0].Measurement.ContentLoss.Event
	lost := loss.Audio.Lost
	if tc.kind == kindVideo {
		lost = loss.Video.Lost
	}
	require.Equal(t, wantLost, lost, report.Moves[0].Measurement.Summary())
}

// gatedAgent echoes audio and records each consumed caller index. Its next
// callback can be held open, which keeps a planned export's flush waiting.
type gatedAgent struct {
	count    uint64
	block    *atomic.Bool
	release  chan struct{}
	consumed *sync.Map // index -> *atomic.Int64, shared by both workers
}

func (a *gatedAgent) Process(ctx context.Context, in agent.Input) (agent.Output, error) {
	if a.block != nil && a.block.CompareAndSwap(true, false) {
		select {
		case <-a.release:
		case <-ctx.Done():
			return agent.Output{}, ctx.Err()
		}
	}
	a.count++
	counter, _ := a.consumed.LoadOrStore(in.Progress.Index&0xffff, new(atomic.Int64))
	counter.(*atomic.Int64).Add(1)
	return agent.Output{Audio: in.Payload, Changed: true}, nil
}
func (a *gatedAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: []byte{byte(a.count)}}, nil
}
func (a *gatedAgent) Restore(_ context.Context, state agent.State) error {
	if state.Version != 1 || len(state.Bytes) != 1 {
		return agent.ErrVersion
	}
	a.count = uint64(state.Bytes[0])
	return nil
}
func (a *gatedAgent) Resume(context.Context, agent.ResumeNotice) error { return nil }

// A custom audio agent flushes its queue before export. Audio that reaches
// the old worker during that flush is not authenticated there; the relay
// replays it and the target's agent consumes it exactly once (ADR 0009).
func TestPlannedMoveAgentFlushRecoversLateAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	consumed := new(sync.Map)
	var block atomic.Bool
	release := make(chan struct{})
	agents := []mediaworker.AgentConfig{
		{Factory: func(string) agent.Agent { return &gatedAgent{block: &block, release: release, consumed: consumed} }, CallbackTimeout: 2 * time.Second},
		{Factory: func(string) agent.Agent { return &gatedAgent{consumed: consumed} }},
	}
	exporting := make(chan struct{}, 1)
	sys := startReorderTopology(t, reorderOptions{buffer: true, agents: agents, workerHandler: func(i int, next http.Handler) http.Handler {
		if i == 0 {
			return observeExport(next, exporting)
		}
		return next
	}})
	call, err := sys.h.Dial(ctx, CallOptions{Worker: 0})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
	settleLeg(t, sys.legs[0], 100*time.Millisecond)
	units := newReorderUnits(t, call)
	block.Store(true)
	units.send(t, kindAudio) // enters the agent and holds its callback open
	captured := captureNext(sys.legs[0], kindAudio, true)
	returned := units.send(t, kindAudio)
	late := <-captured
	sys.legs[0].setIntercept(nil)
	header := capturedHeader(t, late)
	type outcome struct {
		result controlplane.MoveResult
		err    error
	}
	moved := make(chan outcome, 1)
	go func() {
		result, err := sys.plane.Move(ctx, call.SessionID(), "1")
		moved <- outcome{result, err}
	}()
	select {
	case <-exporting:
	case <-ctx.Done():
		t.Fatal("export did not start")
	}
	time.Sleep(20 * time.Millisecond) // the export is flushing the blocked agent
	sys.legs[0].deliver(late)
	time.Sleep(20 * time.Millisecond)
	close(release)
	out := <-moved
	require.NoError(t, out.err)
	result := out.result
	call.RecordExternalMove(result)
	require.NoError(t, call.SendMedia(ctx, 400*time.Millisecond))
	time.Sleep(50 * time.Millisecond)
	require.True(t, returned(), "late audio reached the target's agent")
	require.Equal(t, 1, result.Result.RecoveredPackets)
	counter, ok := consumed.Load(uint64(header.SequenceNumber))
	require.True(t, ok)
	require.EqualValues(t, 1, counter.(*atomic.Int64).Load(), "consumed exactly once across agents")
	require.Zero(t, sys.workers[0].AgentStats().InputDrops, "the paused source never accepted it")
	failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
	require.NoError(t, err)
	require.Zero(t, failures)
	replays, err := sys.workers[1].SessionReplayRejections(call.SessionID())
	require.NoError(t, err)
	require.Zero(t, replays)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	requireNoDuplicates(t, call, report)
	require.Zero(t, report.Moves[0].Measurement.ContentLoss.Event.Audio.Lost)
}

func observeExport(next http.Handler, exporting chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/export") {
			select {
			case exporting <- struct{}{}:
			default:
			}
		}
		next.ServeHTTP(w, req)
	})
}

// A failed target resume rolls the call back to the same export. The late
// original can reach the resumed source before or after its recovered copy;
// either way it is echoed once and the second copy is one replay rejection.
func TestPlannedMoveRollbackRecoversLateInput(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "original before the replay", false: "original after the replay"}[before], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			type delayed struct {
				datagram []byte
				header   rtp.Header
			}
			var pending atomic.Pointer[delayed]
			var sourceLeg atomic.Pointer[privateLeg]
			decrypts := &decryptCounts{counts: make(map[plannedPacketKey]int)}
			var deliveredEarly atomic.Bool
			sys := startReorderTopology(t, reorderOptions{buffer: true, workerHandler: func(i int, next http.Handler) http.Handler {
				if i == 1 {
					return failResume(next)
				}
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, req)
					if late := pending.Load(); req.URL.Path == "/resume" && before && late != nil {
						// The rollback resumed the source; its release has not run yet.
						sourceLeg.Load().deliver(late.datagram)
						for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
							if decrypts.get(late.header) == 1 {
								deliveredEarly.Store(true)
								break
							}
						}
					}
				})
			}})
			sourceLeg.Store(sys.legs[0])
			decrypts.observe(t, sys.workers)
			call, err := sys.h.Dial(ctx, CallOptions{Worker: 0})
			require.NoError(t, err)
			id := call.SessionID()
			require.NoError(t, call.SendMedia(ctx, 300*time.Millisecond))
			settleLeg(t, sys.legs[0], 100*time.Millisecond)
			units := newReorderUnits(t, call)
			captured := captureNext(sys.legs[0], kindAudio, true)
			returned := units.send(t, kindAudio)
			late := <-captured
			sys.legs[0].setIntercept(nil)
			header := capturedHeader(t, late)
			pending.Store(&delayed{datagram: late, header: header})
			require.Eventually(t, func() bool {
				high, ok := sourceHigh(t, sys.workers[0], id, header.SSRC)
				return ok && high == header.SequenceNumber-1
			}, 2*time.Second, time.Millisecond)
			result, err := sys.plane.Move(ctx, id, "1")
			require.ErrorContains(t, err, "rolled back")
			require.True(t, result.Result.RolledBack)
			call.RecordExternalMove(result)
			require.Equal(t, 1, result.Result.RecoveredPackets)
			require.Equal(t, before, deliveredEarly.Load(), "the original reached the resumed source before its release")
			if !before {
				sys.legs[0].deliver(late)
			}
			require.NoError(t, call.SendMedia(ctx, 400*time.Millisecond))
			time.Sleep(50 * time.Millisecond)
			require.True(t, returned())
			require.Equal(t, 1, decrypts.get(header), "authenticated once; the other copy was rejected")
			decrypts.requireOnce(t)
			failures, err := sys.workers[0].SessionDecryptFailures(id)
			require.NoError(t, err)
			replays, err := sys.workers[0].SessionReplayRejections(id)
			require.NoError(t, err)
			require.Zero(t, failures, "a duplicate is not a decryption failure")
			require.EqualValues(t, 1, replays)
			report, err := call.Hangup(ctx)
			require.NoError(t, err)
			require.True(t, report.ConnectedThroughout())
			require.Zero(t, report.DecryptionFailures.Total())
			requireNoDuplicates(t, call, report)
		})
	}
}

func failResume(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/resume" {
			privateapi.Error(w, errInjectedResume, mediaworker.RemoteErrors)
			return
		}
		next.ServeHTTP(w, req)
	})
}

var errInjectedResume = &injectedError{"injected definitive resume failure"}

type injectedError struct{ message string }

func (e *injectedError) Error() string { return e.message }
