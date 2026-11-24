package mediaworker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Characterize allowed ICE transport changes while keeping media pinned.
// These are synthetic Chrome-style offers, not full browser captures.
func TestRestartIdentityCurrentNormalization(t *testing.T) {
	base := testOffer(testOfferAttrs{
		session: []string{"a=extmap-allow-mixed", "a=msid-semantic: WMS stream-1", "a=ice-options:trickle"},
		audio:   []string{"a=sendrecv", "a=rtcp:9 IN IP4 0.0.0.0", "a=msid:stream-1 audio-1", "a=rtcp-rsize", "a=fmtp:111 minptime=10;useinbandfec=1", "a=ssrc:123 cname:caller", "a=ssrc:123 msid:stream-1 audio-1", "a=extmap:1 urn:example:extension", "a=candidate:1 1 udp 100 192.0.2.1 40000 typ host", "a=end-of-candidates"},
		video:   []string{"a=sendrecv", "a=msid:stream-1 video-1", "a=rtpmap:97 rtx/90000", "a=fmtp:97 apt=96", "a=ssrc-group:FID 456 789", "a=ssrc:456 cname:caller", "a=ssrc:789 cname:caller"},
	})
	base = strings.Replace(base, "SAVPF 96\r\n", "SAVPF 96 97\r\n", 1)
	want, err := restartIdentity(base)
	require.NoError(t, err)
	require.Equal(t, "3dd8d076f7c00a7c4c698df7c963321b3b71935794b4d70c222817e3ede16249", want, "workers of one release must agree on the persisted identity bytes")
	for _, tc := range []struct {
		name, from, to string
		accepted       bool
	}{
		{"ice-ufrag", "a=ice-ufrag:" + testCallerUfrag, "a=ice-ufrag:restarted", true},
		{"ice-pwd", "a=ice-pwd:" + testCallerPwd, "a=ice-pwd:replacement-password", true},
		{"origin-version", "o=- 1 1 ", "o=- 1 2 ", true},
		{"setup", "a=setup:actpass", "a=setup:active", true},
		{"extension", "a=extmap:1 urn:example:extension\r\n", "", true},
		{"mixed-extension", "a=extmap-allow-mixed\r\n", "", true},
		{"feedback", "a=rtcp-fb:96 nack pli\r\n", "", true},
		{"candidate", "a=candidate:1 1 udp 100 192.0.2.1 40000 typ host\r\n", "", true},
		{"end-of-candidates", "a=end-of-candidates\r\n", "", true},
		{"origin-id", "o=- 1 1 ", "o=- 2 1 ", false},
		{"origin-address", "o=- 1 1 IN IP4 127.0.0.1", "o=- 1 1 IN IP4 192.0.2.1", false},
		{"origin-user", "o=- 1 1 ", "o=caller 1 1 ", false},
		{"bundle-order", "a=group:BUNDLE 0 1", "a=group:BUNDLE 1 0", false},
		{"mid", "a=mid:1", "a=mid:2", false},
		{"fingerprint", "AB:", "CD:", false},
		{"payload-mapping", "VP8/90000", "VP9/90000", false},
		{"payload-list", "SAVPF 96 97", "SAVPF 96", false},
		{"codec-removal", "a=rtpmap:97 rtx/90000\r\na=fmtp:97 apt=96\r\n", "", false},
		{"codec-order", "SAVPF 96 97", "SAVPF 97 96", false},
		{"fmtp-order", "minptime=10;useinbandfec=1", "useinbandfec=1;minptime=10", false},
		{"ssrc", "a=ssrc:123 ", "a=ssrc:124 ", false},
		{"ssrc-group", "a=ssrc-group:FID 456 789", "a=ssrc-group:FID 456 790", false},
		{"ssrc-msid", "a=ssrc:123 msid:stream-1 audio-1\r\n", "", false},
		{"media-msid", "a=msid:stream-1 audio-1\r\n", "", false},
		{"msid-semantic", "a=msid-semantic: WMS stream-1", "a=msid-semantic: WMS *", false},
		{"attribute-order", "a=sendrecv\r\na=msid:stream-1 video-1", "a=msid:stream-1 video-1\r\na=sendrecv", false},
		{"ice-options", "a=ice-options:trickle", "a=ice-options:trickle renomination", false},
		{"protocol", "UDP/TLS/RTP/SAVPF", "RTP/SAVPF", false},
		{"port-count", "m=video 9 ", "m=video 40000/2 ", true},
		{"session-connection", "s=-\r\n", "s=-\r\nc=IN IP4 192.0.2.1\r\n", true},
		{"rtcp-removal", "a=rtcp:9 IN IP4 0.0.0.0\r\n", "", true},
		{"media-port", "m=audio 9 ", "m=audio 40000 ", true},
		{"media-rejected", "m=video 9 ", "m=video 0 ", false},
		{"connection", "c=IN IP4 0.0.0.0", "c=IN IP4 192.0.2.1", true},
		{"rtcp-address", "a=rtcp:9 IN IP4 0.0.0.0", "a=rtcp:40001 IN IP4 192.0.2.1", true},
		{"bandwidth", "c=IN IP4 0.0.0.0\r\n", "c=IN IP4 0.0.0.0\r\nb=AS:1000\r\n", false},
		{"rtcp-mux-only", "a=rtcp-mux\r\n", "a=rtcp-mux\r\na=rtcp-mux-only\r\n", false},
		{"rtcp-rsize", "a=rtcp-rsize\r\n", "", false},
		{"direction", "a=sendrecv\r\n", "a=sendonly\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.ReplaceAll(base, tc.from, tc.to)
			require.NotEqual(t, base, changed, "mutation must affect the offer")
			got, err := restartIdentity(changed)
			require.NoError(t, err)
			require.Equal(t, tc.accepted, want == got)
		})
	}
	_, err = restartIdentity("not SDP")
	require.ErrorIs(t, err, ErrRestartMismatch)
}

func TestRestartIdentityIgnoresChromeDefaultCandidates(t *testing.T) {
	initial := testOffer(testOfferAttrs{
		audio: []string{"a=ssrc:123 cname:caller", "a=msid:stream-1 audio-1", "a=fmtp:111 minptime=10;useinbandfec=1", "a=rtcp:36574 IN IP4 10.0.141.37"},
		video: []string{"a=ssrc:456 cname:caller", "a=msid:stream-1 video-1", "a=rtcp:36574 IN IP4 10.0.141.37"},
	})
	// Reproduce the transport lines from the CI failure on both m-lines.
	initial = strings.Replace(initial, "SAVPF 111\r\n", "SAVPF 111 63 9 0 8 13 110 126\r\n", 1)
	initial = strings.ReplaceAll(initial, " 9 UDP/TLS/RTP/SAVPF ", " 36573 UDP/TLS/RTP/SAVPF ")
	initial = strings.ReplaceAll(initial, "c=IN IP4 0.0.0.0", "c=IN IP4 10.0.141.37")
	initial = strings.Replace(initial, "t=0 0\r\n", "c=IN IP4 10.0.141.37\r\nt=0 0\r\n", 1)
	want, err := restartIdentity(initial)
	require.NoError(t, err)
	for _, port := range []string{"9", "57630", "57630/2"} {
		t.Run(port, func(t *testing.T) {
			restart := strings.ReplaceAll(initial, " 36573 UDP/TLS/RTP/SAVPF ", " "+port+" UDP/TLS/RTP/SAVPF ")
			restart = strings.ReplaceAll(restart, "c=IN IP4 10.0.141.37", "c=IN IP4 0.0.0.0")
			restart = strings.ReplaceAll(restart, "a=rtcp:36574 IN IP4 10.0.141.37", "a=rtcp:9 IN IP4 0.0.0.0")
			got, err := restartIdentity(restart)
			require.NoError(t, err)
			require.Equal(t, want, got, "placeholder and regathered default candidates preserve identity")
			for _, change := range []struct{ name, from, to string }{
				{"payload-list", "SAVPF 111 63 9 0 8 13 110 126", "SAVPF 111"},
				{"payload-order", "SAVPF 111 63", "SAVPF 63 111"},
				{"ssrc", "a=ssrc:123 ", "a=ssrc:124 "},
				{"mid", "a=mid:1", "a=mid:2"},
				{"fingerprint", "AB:", "CD:"},
				{"protocol", "UDP/TLS/RTP/SAVPF", "RTP/SAVPF"},
			} {
				t.Run(change.name, func(t *testing.T) {
					changed := strings.ReplaceAll(restart, change.from, change.to)
					require.NotEqual(t, restart, changed)
					got, err := restartIdentity(changed)
					require.NoError(t, err)
					require.NotEqual(t, want, got, "media identity stays pinned despite transport changes")
				})
			}
		})
	}
}
