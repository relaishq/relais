package callharness

import (
	"errors"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"
)

// callerICEChecks is how many connectivity checks the caller sends on a
// candidate pair before it gives the pair up. Pion checks every 200 ms
// while connecting, so this keeps checking for about 10 s, as a browser
// does. Pion's default gives up after 8 checks (1.6 s). The ICE-lite server
// never checks back, so nothing revives a pair the caller gave up, and a
// short UDP stall at call start would fail the dial (#122). A server that
// never answers still fails at the dial's own deadline.
const callerICEChecks = 50

// Dial outcomes (DialReport.Outcome).
const (
	DialConnected = "connected"
	DialFailed    = "failed"
)

// Dial failure kinds (DialReport.Failure). The kind comes from what the
// caller observed, so a path that never opened is told apart from a server
// that opened the path and then stalled.
const (
	// DialFailureSetup: the dial failed before it sent its offer.
	DialFailureSetup = "setup"
	// DialFailureSignaling: the offer/answer exchange failed.
	DialFailureSignaling = "signaling"
	// DialFailurePathNeverOpened: no connectivity check got an answer, so
	// the UDP path to the server never completed a round trip.
	DialFailurePathNeverOpened = "path_never_opened"
	// DialFailureICEStalled: some checks were answered, but ICE never
	// connected.
	DialFailureICEStalled = "ice_stalled"
	// DialFailureICEConnectedStalled: ICE connected, but DTLS or the
	// connected state did not follow. The path worked, so this is a product
	// failure.
	DialFailureICEConnectedStalled = "ice_connected_stalled"
	// DialFailureHarnessClosed: the harness closed during the dial.
	DialFailureHarnessClosed = "harness_closed"
)

// DialReport is what the caller observed while one dial set up its call:
// how ICE, DTLS and the PeerConnection progressed, and how the caller's
// connectivity checks were answered. Times are offsets from the start of the
// dial. A failed dial returns it in a *DialError; a connected one keeps it
// in Call.DialReport.
type DialReport struct {
	Outcome string `json:"outcome"`
	// Failure is the kind of a failed dial (DialFailure*); empty if the
	// dial connected. Error is the dial's error.
	Failure string `json:"failure,omitempty"`
	Error   string `json:"error,omitempty"`
	// Session is the call's session ID, the server's ICE username fragment;
	// empty if no answer was applied.
	Session string `json:"session,omitempty"`
	// Elapsed is when the dial connected or failed.
	Elapsed time.Duration `json:"elapsed_ns"`

	// The ICE, DTLS and PeerConnection states when the dial connected or
	// failed, and every change of each before then.
	ICEState         string        `json:"ice_state"`
	DTLSState        string        `json:"dtls_state"`
	ConnectionState  string        `json:"connection_state"`
	ICEStates        []StateChange `json:"ice_states"`
	DTLSStates       []StateChange `json:"dtls_states"`
	ConnectionStates []StateChange `json:"connection_states"`

	// CandidatePairs counts the caller's connectivity checks on each
	// candidate pair, as observed on the caller's socket.
	CandidatePairs []CandidatePairChecks `json:"candidate_pairs"`
}

// CandidatePairChecks counts the binding requests the caller sent on one
// candidate pair and the success responses it received. A zero time means
// the event did not happen.
type CandidatePairChecks struct {
	Local             string        `json:"local"`
	Remote            string        `json:"remote"`
	ChecksSent        uint64        `json:"checks_sent"`
	ResponsesReceived uint64        `json:"responses_received"`
	FirstCheck        time.Duration `json:"first_check_ns,omitempty"`
	LastCheck         time.Duration `json:"last_check_ns,omitempty"`
	LastResponse      time.Duration `json:"last_response_ns,omitempty"`
}

// DialError is every error Dial returns. It keeps the dial's evidence;
// errors.Is and errors.As see the underlying error, and Error returns its
// message unchanged.
type DialError struct {
	Report DialReport
	err    error
}

func (e *DialError) Error() string { return e.err.Error() }
func (e *DialError) Unwrap() error { return e.err }

// DialReport returns what the caller observed while the call connected.
func (c *Call) DialReport() DialReport {
	return c.dialReport
}

// dtlsStateChange records a DTLS transport state change. Pion calls it with
// the transport's lock held, so it must not call back into the transport.
func (c *Call) dtlsStateChange(state webrtc.DTLSTransportState) {
	at := c.rec.since(time.Now())
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	c.dtlsStates = append(c.dtlsStates, StateChange{At: at, State: state.String()})
}

// newDialError reads the evidence of a failed dial. Dial calls it before it
// closes the call, so the teardown does not appear in the histories. call is
// nil if the dial failed before it made one.
func newDialError(err error, h *Harness, rec *recorder, call *Call) *DialError {
	report := dialReport(rec, call)
	report.Outcome = DialFailed
	report.Error = err.Error()

	h.callsMu.Lock()
	closed := h.closed || errors.Is(err, ErrHarnessClosed)
	h.callsMu.Unlock()
	rec.mu.Lock()
	offered, negotiated := rec.offerAnswerExchanges > 0, rec.negotiated
	rec.mu.Unlock()
	report.Failure = dialFailure(closed, offered, negotiated, report)

	return &DialError{Report: report, err: err}
}

// dialFailure classifies a failed dial by how far the caller got. A dial
// the harness closed under it is not a path or server failure.
func dialFailure(closed, offered, negotiated bool, report DialReport) string {
	var answers uint64
	for _, pair := range report.CandidatePairs {
		answers += pair.ResponsesReceived
	}
	iceConnected := slices.ContainsFunc(report.ICEStates, func(change StateChange) bool {
		return change.State == webrtc.ICEConnectionStateConnected.String() ||
			change.State == webrtc.ICEConnectionStateCompleted.String()
	})
	switch {
	case closed:
		return DialFailureHarnessClosed
	case !offered:
		return DialFailureSetup
	case !negotiated:
		return DialFailureSignaling
	case iceConnected:
		return DialFailureICEConnectedStalled
	case answers == 0:
		return DialFailurePathNeverOpened
	default:
		return DialFailureICEStalled
	}
}

// dialReport collects the states, histories and check counts so far.
func dialReport(rec *recorder, call *Call) DialReport {
	report := DialReport{Elapsed: rec.since(time.Now())}
	rec.mu.Lock()
	report.ICEStates = slices.Clone(rec.iceStates)
	report.ConnectionStates = slices.Clone(rec.connectionStates)
	rec.mu.Unlock()
	if call == nil {
		return report
	}

	report.Session = call.remoteUfrag
	report.ICEState = call.pc.ICEConnectionState().String()
	report.ConnectionState = call.pc.ConnectionState().String()
	// State takes the transport's lock; dtlsStateChange runs under it and
	// takes dialMu, so read the state before taking dialMu.
	report.DTLSState = call.pc.SCTP().Transport().State().String()
	call.dialMu.Lock()
	report.DTLSStates = slices.Clone(call.dtlsStates)
	call.dialMu.Unlock()
	report.CandidatePairs = call.socket.observer.candidatePairs()

	return report
}
