package callharness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
)

// workers are the system's media workers. A single worker owns its UDP
// socket. Two or more share one socket (mediaworker.Socket), calls start on
// the first, and a call can move between them. With Options.Relay, each
// worker owns a private socket behind a relay instead (topology.go).
type workers struct {
	socket    *mediaworker.Socket // nil for a single worker
	relay     *relayTopology      // nil without Options.Relay
	list      []*mediaworker.Worker
	signaling http.Handler
}

func startWorkers(opts Options) (*workers, error) {
	if opts.Workers < 0 {
		return nil, fmt.Errorf("callharness: %d workers", opts.Workers)
	}
	if opts.Relay {
		return startRelayedWorkers(opts)
	}
	frames := opts.FrameCache
	if frames == nil {
		frames = framecache.NewMemory(framecache.Limits{})
	}
	cfg := mediaworker.Config{FrameCache: frames, LoggerFactory: opts.WorkerLoggerFactory, DisableFrameCache: opts.DisableFrameCache, DisableResumePLI: opts.DisableResumePLI}
	if opts.Workers <= 1 {
		cfg.ListenAddr = "127.0.0.1:0"
		worker, err := mediaworker.New(cfg)
		if err != nil {
			return nil, err
		}

		return &workers{list: []*mediaworker.Worker{worker}, signaling: worker.SignalingHandler()}, nil
	}

	socket, err := mediaworker.ListenSocket(mediaworker.SocketConfig{
		ListenAddr:    "127.0.0.1:0",
		LoggerFactory: opts.WorkerLoggerFactory,
	})
	if err != nil {
		return nil, err
	}
	ws := &workers{socket: socket}
	for range opts.Workers {
		worker, err := socket.NewWorker(cfg)
		if err != nil {
			return nil, errors.Join(err, ws.close())
		}
		ws.list = append(ws.list, worker)
	}
	ws.signaling = socket.SignalingHandler(ws.list[0])

	return ws, nil
}

// close closes the workers, then their shared socket or relay.
func (ws *workers) close() error {
	var errs []error
	for _, worker := range ws.list {
		errs = append(errs, worker.Close())
	}
	if ws.socket != nil {
		errs = append(errs, ws.socket.Close())
	}
	if ws.relay != nil {
		errs = append(errs, ws.relay.close())
	}

	return errors.Join(errs...)
}

func (ws *workers) index(worker *mediaworker.Worker) int {
	return slices.Index(ws.list, worker)
}

// HandoverOptions shape a planned handover.
type HandoverOptions struct {
	// To is the index of the worker that takes the call over (0 is the
	// worker calls start on).
	To int

	// SequenceMargin moves the echoed tracks' sequence numbers forward by
	// this much across the move; see mediaworker.ResumeOptions. Zero, the
	// right value for a planned handover, keeps them continuous.
	SequenceMargin uint16
}

// Handover moves the call to another worker through the relay control
// plane or on a shared socket. The old worker exports the session to bytes
// and the new one resumes them. It needs Options.Workers of 2 or more. The
// report describes each move as the caller saw it (Report.Moves).
func (c *Call) Handover(opts HandoverOptions) error {
	ws := c.harness.workers
	if ws.socket == nil && (ws.relay == nil || len(ws.list) < 2) {
		return errors.New("callharness: a handover needs Options.Workers of 2 or more")
	}
	if opts.To < 0 || opts.To >= len(ws.list) {
		return fmt.Errorf("callharness: no worker %d", opts.To)
	}
	sessionID, err := c.sessionID()
	if err != nil {
		return err
	}

	if ws.relay != nil {
		if opts.SequenceMargin != 0 {
			return errors.New("callharness: relay planned moves require sequence margin 0")
		}
		t := ws.relay
		started := time.Now()
		owner, _ := c.harness.SessionOwner(sessionID)
		result, err := t.plane.Move(context.Background(), sessionID, strconv.Itoa(opts.To))
		if result.Start.IsZero() {
			result.Start = started
			result.End = time.Now()
			result.From = strconv.Itoa(owner)
		}
		from, _ := strconv.Atoi(result.From)
		c.rec.move(moveRecord{from: from, to: opts.To, start: result.Start, end: result.End, result: result.Result, err: err})
		if err != nil {
			return fmt.Errorf("callharness: relay move: %w", err)
		}
		return nil
	}

	from := ws.index(ws.socket.Owner(sessionID))
	start := time.Now()
	result, err := ws.socket.Handover(sessionID, ws.list[opts.To], mediaworker.ResumeOptions{
		SequenceMargin: opts.SequenceMargin,
	})
	end := time.Now()
	c.rec.move(moveRecord{from: from, to: opts.To, start: start, end: end, result: result, err: err})
	if err != nil {
		return fmt.Errorf("callharness: handover to worker %d: %w", opts.To, err)
	}

	return nil
}

// sessionID is the call's session ID, the last segment of its resource URL.
func (c *Call) sessionID() (string, error) {
	resource, err := url.Parse(c.resourceURL)
	if err != nil || c.resourceURL == "" {
		return "", fmt.Errorf("callharness: no session ID in resource URL %q", c.resourceURL)
	}

	return path.Base(resource.Path), nil
}

// moveRecord is one handover the harness made.
type moveRecord struct {
	kind       string
	detection  time.Duration
	from, to   int
	start, end time.Time
	result     mediaworker.HandoverResult
	err        error
}

// rtpMark is the header of a packet the caller received.
type rtpMark struct {
	seq           uint16
	timestamp     uint32
	pictureID     uint16
	havePictureID bool
}

// frameMark is a complete video frame the caller received.
type frameMark struct {
	at           time.Duration
	decodable    bool
	firstArrival time.Duration
	source       sentVideoFrame
	pictureID    uint16
}

// consentSample is the caller's running totals of consent checks sent and
// answered, at one of them (see consent.go).
type consentSample struct {
	at                  time.Duration
	requests, responses uint64
}

func (r *recorder) move(record moveRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.moves = append(r.moves, record)
	// Automatic events are collected at hangup, after any later planned move
	// was recorded. Keep the report and consent window in event order.
	sort.SliceStable(r.moves, func(i, j int) bool { return r.moves[i].start.Before(r.moves[j].start) })
}

// MoveReport is one planned handover as the caller observed it, plus what
// the system reported about it.
type MoveReport struct {
	// Recovery measures video recovery from the caller's first post-margin RTP
	// packet, including replay received while ResumeSession is still returning.
	Recovery VideoRecovery

	Kind                          string        // "move" or "takeover"
	DetectionTime                 time.Duration // failure action to control-plane detection
	DecryptionFailuresAfterResume int
	From, To                      int // worker indexes

	// Start and End bracket the handover call, as offsets from dialing.
	Start, End time.Duration

	// Error is the handover's error; empty when it succeeded.
	Error string

	// Result is the system's account of the move: exported state size,
	// caller packets held and timings.
	Result mediaworker.HandoverResult

	// Tracks describe each received track around the move.
	Tracks []MoveTrackReport
}

// MoveTrackReport is one received track around a move. "Around the move"
// runs from the last packet that arrived before the move started to the
// first that arrived after it ended.
type MoveTrackReport struct {
	Kind string

	// Gap is the longest interval between consecutive packets around the
	// move: the media gap the move caused. TypicalInterval is the track's
	// median interval between packets, for comparison.
	Gap             time.Duration
	TypicalInterval time.Duration

	// SkippedSequenceNumbers counts sequence numbers missing around the
	// move: a sequence margin's jump, or lost packets.
	SkippedSequenceNumbers int

	// LargestTimestampStep is the largest RTP timestamp step between
	// consecutive packets around the move; TypicalTimestampStep is the
	// track's median step between packets of different frames. With
	// continuous timestamps the first is no larger than one frame's step.
	LargestTimestampStep uint32
	TypicalTimestampStep uint32

	// PacketsAfter counts packets that arrived after the move ended.
	PacketsAfter int

	// FirstDecodableFrameAfter is, for video, the time from the end of the
	// move to the first decodable frame after it; zero if none arrived.
	FirstDecodableFrameAfter time.Duration
}

// ConsentReport is what the caller observed of its ICE consent checks: the
// STUN binding requests it sent and the binding success responses to them,
// seen on its own UDP socket (see consent.go).
type ConsentReport struct {
	// RequestsSent and ResponsesReceived are the totals until hangup.
	RequestsSent      uint64
	ResponsesReceived uint64

	// Since is when the window below starts: the end of the last move, or
	// when the call connected if nothing moved. ObservedFor runs from there
	// to hangup, or to when the caller's connection closed or failed if that
	// came first.
	Since       time.Duration
	ObservedFor time.Duration

	// ResponsesAfter counts responses received in the window, and
	// LongestWithoutResponse is the longest stretch of it without a new
	// response, the stretch from the last response to the end of the window
	// included. The caller checks every 2 s; with no packet at all for 5 s
	// its connection goes "disconnected".
	ResponsesAfter         uint64
	LongestWithoutResponse time.Duration
}

// moveReports describes every move from the caller's records. It runs under
// r.mu.
func (r *recorder) moveReports() []MoveReport {
	reports := make([]MoveReport, 0, len(r.moves))
	for _, move := range r.moves {
		report := MoveReport{
			Kind:          move.kind,
			DetectionTime: move.detection,
			From:          move.from,
			To:            move.to,
			Start:         r.since(move.start),
			End:           r.since(move.end),
			Result:        move.result,
		}
		if report.Kind == "" {
			report.Kind = "move"
		}
		if move.err != nil {
			report.Error = move.err.Error()
		}
		for _, track := range r.tracks {
			report.Tracks = append(report.Tracks, track.aroundMove(report.Start, report.End))
		}
		if report.Kind == "takeover" {
			limit := r.hungUpAt
			if len(reports)+1 < len(r.moves) {
				limit = r.since(r.moves[len(reports)+1].start)
			}
			report.Recovery = r.videoRecovery(report.Start, limit)
		}
		firstResumed := time.Duration(0)
		for _, track := range r.tracks {
			i := sort.Search(len(track.arrivals), func(i int) bool { return track.arrivals[i] > report.End })
			if i < len(track.arrivals) && (firstResumed == 0 || track.arrivals[i] < firstResumed) {
				firstResumed = track.arrivals[i]
			}
		}
		if firstResumed != 0 {
			for _, at := range r.decryptFailureTimes {
				if at >= firstResumed {
					report.DecryptionFailuresAfterResume++
				}
			}
		}
		reports = append(reports, report)
	}

	return reports
}

func (t *trackRecord) aroundMove(start, end time.Duration) MoveTrackReport {
	report := MoveTrackReport{
		Kind:                 t.kind,
		TypicalInterval:      medianInterval(t.arrivals),
		TypicalTimestampStep: medianTimestampStep(t.headers),
	}

	// first: the last arrival before the move started; after: the first
	// arrival after it ended.
	first := max(sort.Search(len(t.arrivals), func(i int) bool { return t.arrivals[i] > start })-1, 0)
	after := sort.Search(len(t.arrivals), func(i int) bool { return t.arrivals[i] > end })
	report.PacketsAfter = len(t.arrivals) - after

	for i := first; i < after && i+1 < len(t.arrivals); i++ {
		report.Gap = max(report.Gap, t.arrivals[i+1]-t.arrivals[i])
		if step := int16(t.headers[i+1].seq - t.headers[i].seq); step > 1 { //nolint:gosec // wraps on purpose
			report.SkippedSequenceNumbers += int(step) - 1
		}
		if step := t.headers[i+1].timestamp - t.headers[i].timestamp; int32(step) > 0 { //nolint:gosec // wraps on purpose
			report.LargestTimestampStep = max(report.LargestTimestampStep, step)
		}
	}

	if t.video != nil {
		for _, frame := range t.video.frames {
			if frame.at > end && frame.decodable {
				report.FirstDecodableFrameAfter = frame.at - end

				break
			}
		}
	}

	return report
}

func medianInterval(arrivals []time.Duration) time.Duration {
	if len(arrivals) < 2 {
		return 0
	}
	intervals := make([]time.Duration, 0, len(arrivals)-1)
	for i := 1; i < len(arrivals); i++ {
		intervals = append(intervals, arrivals[i]-arrivals[i-1])
	}
	slices.Sort(intervals)

	return intervals[len(intervals)/2]
}

// medianTimestampStep is the median positive timestamp step: packets of the
// same video frame share a timestamp and are left out.
func medianTimestampStep(headers []rtpMark) uint32 {
	var steps []uint32
	for i := 1; i < len(headers); i++ {
		if step := headers[i].timestamp - headers[i-1].timestamp; int32(step) > 0 { //nolint:gosec // wraps on purpose
			steps = append(steps, step)
		}
	}
	if len(steps) == 0 {
		return 0
	}
	slices.Sort(steps)

	return steps[len(steps)/2]
}

// consentReport summarizes the consent samples. It runs under r.mu.
func (r *recorder) consentReport() ConsentReport {
	report := ConsentReport{Since: r.connectedAt}
	if n := len(r.moves); n > 0 {
		report.Since = r.since(r.moves[n-1].end)
	}
	end := r.consentEnd()
	if n := len(r.consent); n > 0 {
		last := r.consent[n-1]
		report.RequestsSent, report.ResponsesReceived = last.requests, last.responses
	}

	var before uint64 // responses at the start of the window
	lastResponse := report.Since
	for _, sample := range r.consent {
		if sample.at > end {
			break
		}
		if sample.at <= report.Since {
			before = sample.responses

			continue
		}
		if sample.responses > before+report.ResponsesAfter {
			report.LongestWithoutResponse = max(report.LongestWithoutResponse, sample.at-lastResponse)
			report.ResponsesAfter = sample.responses - before
			lastResponse = sample.at
		}
	}
	if end > report.Since {
		// The silent tail up to the end of the window counts too.
		report.ObservedFor = end - report.Since
		report.LongestWithoutResponse = max(report.LongestWithoutResponse, end-lastResponse)
	}

	return report
}

// consentEnd is when the consent window ends: at hangup, or when the
// caller's connection closed or failed, if that came first. It runs under
// r.mu.
func (r *recorder) consentEnd() time.Duration {
	end := r.hungUpAt
	for _, change := range r.connectionStates {
		if change.At > r.connectedAt && (change.State == "closed" || change.State == "failed") {
			return min(end, change.At)
		}
	}

	return end
}

func writeHandoverSummary(b *strings.Builder, r *Report) {
	for i, move := range r.Moves {
		res := move.Result
		if move.Kind == "takeover" {
			recovery := move.Recovery
			live := recovery.FirstDecodedLiveAfterKill.String()
			if recovery.LivePath == "" {
				live = "not observed"
			}
			fmt.Fprintf(b, "  video recovery: first decoded %s, first live %s from kill; %s from media resume; path=%s live_path=%s picture_id=%d interval=%s keyframe_interval=%s within_interval=%t\n", recovery.FirstDecodedAfterKill, live, recovery.FirstDecodedAfterMediaResume, recovery.Path, recovery.LivePath, recovery.PictureID, recovery.FrameInterval, recovery.KeyframeInterval, recovery.WithinFrameInterval)
		}
		fmt.Fprintf(b, "  %s %d: worker %d -> %d at %s (detection %s, decrypt failures after resume %d)", move.Kind, i+1, move.From, move.To, ms(move.Start), ms(move.DetectionTime), move.DecryptionFailuresAfterResume)
		if move.Error != "" {
			fmt.Fprintf(b, " FAILED (rolled back: %t): %s\n", res.RolledBack, move.Error)
		} else {
			fmt.Fprintf(b, "; handover %s (drain %s, export %s, resume %s), state %d B, %d caller packets held\n",
				us(res.Duration), us(res.Drain), us(res.Export), us(res.Resume), res.StateBytes, res.HeldPackets)
		}
		if res.HoldExpired {
			b.WriteString("    warning: caller hold expired; packets auto-released\n")
		}
		for _, t := range move.Tracks {
			fmt.Fprintf(b, "    %s: gap %s (typical %s), %d seq skipped, largest ts step %d (typical %d), %d packets after",
				t.Kind, ms(t.Gap), ms(t.TypicalInterval), t.SkippedSequenceNumbers,
				t.LargestTimestampStep, t.TypicalTimestampStep, t.PacketsAfter)
			if t.Kind == kindVideo {
				fmt.Fprintf(b, ", first decodable frame %s after", ms(t.FirstDecodableFrameAfter))
			}
			b.WriteString("\n")
		}
	}
	c := r.Consent
	fmt.Fprintf(b, "  consent checks:   %d sent, %d answered; from %s for %s: %d answered, longest without an answer %s\n",
		c.RequestsSent, c.ResponsesReceived, ms(c.Since), ms(c.ObservedFor), c.ResponsesAfter, ms(c.LongestWithoutResponse))
}

func us(d time.Duration) string {
	return d.Round(10 * time.Microsecond).String()
}
