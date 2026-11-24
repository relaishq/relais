package mediaworker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Characterize the existing byte-level identity before adding diagnostics.
// These are synthetic Chrome-style offers, not captures of the failing run.
func TestRestartIdentityCurrentNormalization(t *testing.T) {
	base := testOffer(testOfferAttrs{
		session: []string{"a=extmap-allow-mixed", "a=msid-semantic: WMS stream-1", "a=ice-options:trickle"},
		audio:   []string{"a=sendrecv", "a=rtcp:9 IN IP4 0.0.0.0", "a=msid:stream-1 audio-1", "a=rtcp-rsize", "a=fmtp:111 minptime=10;useinbandfec=1", "a=ssrc:123 cname:caller", "a=ssrc:123 msid:stream-1 audio-1", "a=extmap:1 urn:example:extension", "a=candidate:1 1 udp 100 192.0.2.1 40000 typ host", "a=end-of-candidates"},
		video:   []string{"a=sendrecv", "a=msid:stream-1 video-1", "a=rtpmap:97 rtx/90000", "a=fmtp:97 apt=96", "a=ssrc-group:FID 456 789", "a=ssrc:456 cname:caller", "a=ssrc:789 cname:caller"},
	})
	base = strings.Replace(base, "SAVPF 96\r\n", "SAVPF 96 97\r\n", 1)
	want, err := restartIdentity(base)
	require.NoError(t, err)
	require.Equal(t, "543d1bab41afd613f467ce8a69c2a53610bed5d73b06e0ff12510dfe5370c3cf", want, "persisted identity bytes must not change")
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
		{"media-port", "m=audio 9 ", "m=audio 40000 ", false},
		{"connection", "c=IN IP4 0.0.0.0", "c=IN IP4 192.0.2.1", false},
		{"rtcp-address", "a=rtcp:9 IN IP4 0.0.0.0", "a=rtcp:40001 IN IP4 192.0.2.1", false},
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
