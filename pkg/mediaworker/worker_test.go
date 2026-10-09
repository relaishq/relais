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
		offer        testOfferAttrs
		audio, video bool // whether the answer accepts each m-line
	}{
		{name: "no direction is sendrecv", audio: true, video: true},
		{
			name:  "media sendrecv",
			offer: testOfferAttrs{audio: []string{"a=sendrecv"}, video: []string{"a=sendrecv"}},
			audio: true, video: true,
		},
		{name: "audio recvonly", offer: testOfferAttrs{audio: []string{"a=recvonly"}}, video: true},
		{name: "video sendonly", offer: testOfferAttrs{video: []string{"a=sendonly"}}, audio: true},
		{name: "both inactive", offer: testOfferAttrs{audio: []string{"a=inactive"}, video: []string{"a=inactive"}}},
		{name: "session sendonly", offer: testOfferAttrs{session: []string{"a=sendonly"}}},
		{name: "session recvonly", offer: testOfferAttrs{session: []string{"a=recvonly"}}},
		{name: "session inactive", offer: testOfferAttrs{session: []string{"a=inactive"}}},
		{
			name:  "audio sendrecv overrides session recvonly",
			offer: testOfferAttrs{session: []string{"a=recvonly"}, audio: []string{"a=sendrecv"}},
			audio: true,
		},
		{
			name:  "video sendrecv overrides session inactive",
			offer: testOfferAttrs{session: []string{"a=inactive"}, video: []string{"a=sendrecv"}},
			video: true,
		},
	}

	worker := newTestWorker(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, answer, err := worker.CreateSession(context.Background(), testOffer(tc.offer))
			if !tc.audio && !tc.video {
				require.ErrorIs(t, err, ErrUnsupportedOffer)

				return
			}
			require.NoError(t, err)
			accepted := acceptedMedia(t, answer)
			require.Equal(t, tc.audio, accepted[mediaAudio], "audio accepted")
			require.Equal(t, tc.video, accepted[mediaVideo], "video accepted")
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
	id, answerSDP, err := worker.CreateSession(context.Background(), testOffer(testOfferAttrs{}))
	require.NoError(t, err)
	require.Equal(t, map[string]bool{mediaAudio: true, mediaVideo: true}, acceptedMedia(t, answerSDP),
		"answer accepts audio and video")

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

// testOfferAttrs are extra attribute lines for testOffer: session-level, and
// on the audio and video m-lines.
type testOfferAttrs struct {
	session, audio, video []string
}

// testOffer is a minimal offer for a sendrecv Opus m-line and a sendrecv VP8
// m-line in one BUNDLE group, with extra attribute lines.
func testOffer(attrs testOfferAttrs) string {
	lines := []string{
		"v=0",
		"o=- 1 1 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"a=group:BUNDLE 0 1",
	}
	lines = append(lines, attrs.session...)
	lines = append(lines, "m=audio 9 UDP/TLS/RTP/SAVPF 111")
	lines = append(lines, testMediaTransport("0")...)
	lines = append(lines, "a=rtpmap:111 opus/48000/2")
	lines = append(lines, attrs.audio...)
	lines = append(lines, "m=video 9 UDP/TLS/RTP/SAVPF 96")
	lines = append(lines, testMediaTransport("1")...)
	lines = append(lines, "a=rtpmap:96 VP8/90000", "a=rtcp-fb:96 nack pli")
	lines = append(lines, attrs.video...)

	return strings.Join(lines, "\r\n") + "\r\n"
}

// testMediaTransport is the transport attributes a browser puts on each
// bundled m-line.
func testMediaTransport(mid string) []string {
	return []string{
		"c=IN IP4 0.0.0.0",
		"a=mid:" + mid,
		"a=ice-ufrag:" + testCallerUfrag,
		"a=ice-pwd:" + testCallerPwd,
		"a=fingerprint:sha-256 " + strings.TrimSuffix(strings.Repeat("AB:", 32), ":"),
		"a=setup:actpass",
		"a=rtcp-mux",
	}
}

// acceptedMedia reports, by media type, which m-lines an answer accepts.
func acceptedMedia(t *testing.T, answerSDP string) map[string]bool {
	t.Helper()

	var answer sdp.SessionDescription
	require.NoError(t, answer.UnmarshalString(answerSDP))
	accepted := map[string]bool{}
	for _, md := range answer.MediaDescriptions {
		accepted[md.MediaName.Media] = md.MediaName.Port.Value != 0
	}

	return accepted
}
