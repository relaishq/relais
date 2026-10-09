package mediaworker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/require"
)

// These tests drive a media worker the way a caller does, below the call
// harness: an SDP offer through CreateSession, then ICE connectivity checks
// sent from the caller's own UDP sockets. They check only what that caller
// can see: whether its offer is accepted and whether its checks are answered.
// A worker answers checks only for a live session, so a check that goes
// unanswered means the session has closed.
//
// The call harness's Pion caller cannot send checks from an address of the
// test's choosing, which is why the consent tests live here.

const (
	// testConsentTimeout stands in for RFC 7675's 30 s so the tests run
	// quickly.
	testConsentTimeout = 500 * time.Millisecond

	// testCheckInterval leaves ten checks per consent window, so a slow
	// runner under the race detector does not miss a refresh.
	testCheckInterval = testConsentTimeout / 10

	// testCheckTimeout is how long a check waits for its response.
	testCheckTimeout = time.Second

	testCallerUfrag = "callerufrag"
	testCallerPwd   = "callerpasswordcallerpassword"
)

func TestConsentFreshness(t *testing.T) {
	t.Run("checks from the nominated address keep the session", func(t *testing.T) {
		call := dialTestSession(t)
		nominated := call.endpoint(t)

		require.True(t, nominated.check(t, true), "nomination answered")
		nominated.keepChecking(t, 3*testConsentTimeout)
		require.NoError(t, call.worker.EndSession(call.id), "session still open")
	})

	t.Run("checks from another address do not refresh consent", func(t *testing.T) {
		call := dialTestSession(t)
		nominated, other := call.endpoint(t), call.endpoint(t)

		require.True(t, nominated.check(t, true), "nomination answered")
		require.True(t, other.check(t, false), "valid check from another address answered")
		other.checkUntilClosed(t, 10*testConsentTimeout)
	})

	t.Run("re-nomination moves consent to the new address", func(t *testing.T) {
		call := dialTestSession(t)
		first, second := call.endpoint(t), call.endpoint(t)

		require.True(t, first.check(t, true), "nomination answered")
		require.True(t, second.check(t, true), "re-nomination answered")
		second.keepChecking(t, 3*testConsentTimeout)
		first.checkUntilClosed(t, 10*testConsentTimeout)
	})
}

func TestOfferDirection(t *testing.T) {
	tests := []struct {
		name         string
		sessionAttrs []string
		mediaAttrs   []string
		accepted     bool
	}{
		{name: "no direction is sendrecv", accepted: true},
		{name: "media sendrecv", mediaAttrs: []string{"a=sendrecv"}, accepted: true},
		{name: "media recvonly", mediaAttrs: []string{"a=recvonly"}},
		{name: "session sendonly", sessionAttrs: []string{"a=sendonly"}},
		{name: "session recvonly", sessionAttrs: []string{"a=recvonly"}},
		{name: "session inactive", sessionAttrs: []string{"a=inactive"}},
		{
			name:         "media sendrecv overrides session recvonly",
			sessionAttrs: []string{"a=recvonly"},
			mediaAttrs:   []string{"a=sendrecv"},
			accepted:     true,
		},
	}

	worker := newTestWorker(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := worker.CreateSession(context.Background(), testOffer(tc.sessionAttrs, tc.mediaAttrs))
			if tc.accepted {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrUnsupportedOffer)
			}
		})
	}
}

func newTestWorker(t *testing.T) *Worker {
	t.Helper()

	worker, err := New(Config{consentTimeout: testConsentTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })

	return worker
}

// testCall is a caller's session on a test worker, known from the answer.
type testCall struct {
	worker   *Worker
	id       string
	username string // STUN USERNAME: the worker's ufrag ":" the caller's
	pwd      string // the worker's ICE password, which keys every check
}

func dialTestSession(t *testing.T) *testCall {
	t.Helper()

	worker := newTestWorker(t)
	id, answerSDP, err := worker.CreateSession(context.Background(), testOffer(nil, nil))
	require.NoError(t, err)

	var answer sdp.SessionDescription
	require.NoError(t, answer.UnmarshalString(answerSDP))
	var ufrag, pwd string
	for _, md := range answer.MediaDescriptions {
		if md.MediaName.Port.Value == 0 {
			continue
		}
		ufrag, _ = md.Attribute("ice-ufrag")
		pwd, _ = md.Attribute("ice-pwd")
	}
	require.NotEmpty(t, ufrag, "answer ice-ufrag")
	require.NotEmpty(t, pwd, "answer ice-pwd")

	return &testCall{worker: worker, id: id, username: ufrag + ":" + testCallerUfrag, pwd: pwd}
}

// testEndpoint is one of the caller's addresses (a local candidate).
type testEndpoint struct {
	call *testCall
	conn *net.UDPConn
}

func (c *testCall) endpoint(t *testing.T) *testEndpoint {
	t.Helper()

	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &testEndpoint{call: c, conn: conn}
}

// check sends an authenticated binding request, nominating this address when
// nominate is set, and reports whether the worker answered it with success.
func (e *testEndpoint) check(t *testing.T, nominate bool) bool {
	t.Helper()

	setters := []stun.Setter{
		stun.BindingRequest,
		stun.NewTransactionIDSetter(stun.NewTransactionID()),
		stun.NewUsername(e.call.username),
	}
	if nominate {
		setters = append(setters, stun.RawAttribute{Type: stun.AttrUseCandidate})
	}
	setters = append(setters, stun.NewShortTermIntegrity(e.call.pwd), stun.Fingerprint)
	request, err := stun.Build(setters...)
	require.NoError(t, err)

	_, err = e.conn.WriteToUDPAddrPort(request.Raw, e.call.worker.MediaAddr())
	require.NoError(t, err)

	require.NoError(t, e.conn.SetReadDeadline(time.Now().Add(testCheckTimeout)))
	buf := make([]byte, receiveMTU)
	for {
		n, err := e.conn.Read(buf)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return false
		}
		require.NoError(t, err)

		response := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if response.Decode() != nil || response.TransactionID != request.TransactionID {
			continue // a late response to an earlier check, or not STUN
		}

		return response.Type == stun.BindingSuccess
	}
}

// keepChecking sends consent checks from this address for d and requires
// every one to be answered.
func (e *testEndpoint) keepChecking(t *testing.T, d time.Duration) {
	t.Helper()

	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(testCheckInterval) {
		require.True(t, e.check(t, false), "check from %s answered", e.conn.LocalAddr())
	}
}

// checkUntilClosed sends consent checks from this address until one goes
// unanswered, and requires that to happen within d because the session has
// closed.
func (e *testEndpoint) checkUntilClosed(t *testing.T, d time.Duration) {
	t.Helper()

	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(testCheckInterval) {
		if !e.check(t, false) {
			require.ErrorIs(t, e.call.worker.EndSession(e.call.id), ErrUnknownSession, "session closed")

			return
		}
	}
	t.Fatalf("checks from %s kept the session open for %s; consent timeout is %s",
		e.conn.LocalAddr(), d, testConsentTimeout)
}

// testOffer is a minimal offer for one sendrecv Opus m-line, with extra
// session-level and media-level attribute lines.
func testOffer(sessionAttrs, mediaAttrs []string) string {
	lines := []string{
		"v=0",
		"o=- 1 1 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"a=group:BUNDLE 0",
	}
	lines = append(lines, sessionAttrs...)
	lines = append(lines,
		"m=audio 9 UDP/TLS/RTP/SAVPF 111",
		"c=IN IP4 0.0.0.0",
		"a=mid:0",
		"a=ice-ufrag:"+testCallerUfrag,
		"a=ice-pwd:"+testCallerPwd,
		"a=fingerprint:sha-256 "+strings.TrimSuffix(strings.Repeat("AB:", 32), ":"),
		"a=setup:actpass",
		"a=rtcp-mux",
		"a=rtpmap:111 opus/48000/2",
	)
	lines = append(lines, mediaAttrs...)

	return strings.Join(lines, "\r\n") + "\r\n"
}
