package mediaworker

import (
	"testing"

	"github.com/pion/srtp/v3"
)

// BenchmarkRestoreInbound compares priming only the high water mark with
// restoring a full 64-index receive window on resume (ADR 0009).
func BenchmarkRestoreInbound(b *testing.B) {
	t := &testing.T{}
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	for _, window := range []bool{false, true} {
		b.Run(map[bool]string{false: "high-water-mark", true: "full-window"}[window], func(b *testing.B) {
			for range b.N {
				in, err := srtp.CreateContext(keys.RemoteMasterKey, keys.RemoteMasterSalt, profile, srtp.SRTPReplayProtection(replayWindow))
				if err != nil {
					b.Fatal(err)
				}
				if window {
					err = restoreInboundWindow(in, keys, profile, 456, 70000, ^uint64(0))
				} else {
					err = restoreInboundIndex(in, keys, profile, 456, 70000)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
