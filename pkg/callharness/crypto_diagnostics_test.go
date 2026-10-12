package callharness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

func TestCryptoDiagnosticWrongROC(t *testing.T) {
	for _, profile := range []srtp.ProtectionProfile{srtp.ProtectionProfileAes128CmHmacSha1_80, srtp.ProtectionProfileAeadAes128Gcm} {
		t.Run(profile.String(), func(t *testing.T) {
			keyLen, err := profile.KeyLen()
			require.NoError(t, err)
			saltLen, err := profile.SaltLen()
			require.NoError(t, err)
			key, salt := make([]byte, keyLen), make([]byte, saltLen)
			observer := newCryptoObserver(time.Now())
			observer.profile, observer.keys = profile, srtp.SessionKeys{RemoteMasterKey: key, RemoteMasterSalt: salt}
			observer.receiver, err = srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(64))
			require.NoError(t, err)
			observer.stats.Ready = true
			sender, err := srtp.CreateContext(key, salt, profile)
			require.NoError(t, err)
			encrypt := func(context *srtp.Context, seq uint16) []byte {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 42, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte("private-media-never-in-report")}).Marshal()
				require.NoError(t, err)
				encrypted, err := context.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
				return encrypted
			}
			observer.observe(encrypt(sender, 1000))
			wrong, err := srtp.CreateContext(key, salt, profile)
			require.NoError(t, err)
			wrong.SetROC(42, 1)
			observer.observe(encrypt(wrong, 1001))
			// The alternate-ROC probe must not accept/deliver the failing packet or
			// alter the receiver. Its next valid ROC-zero packet still authenticates.
			observer.observe(encrypt(sender, 1002))
			report := observer.report()
			require.Equal(t, DecryptionFailures{AuthTag: 1}, report.Failures)
			require.Len(t, report.Samples, 1)
			sample := report.Samples[0]
			require.EqualValues(t, 1000, sample.PreviousIndex)
			require.EqualValues(t, 1001, sample.EstimatedIndex)
			require.NotNil(t, sample.AuthenticatedIndex)
			require.EqualValues(t, 65536+1001, *sample.AuthenticatedIndex)
			require.EqualValues(t, 1002, observer.highest[42])
			raw, err := json.Marshal(report)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-media-never-in-report")
			// Bound retained evidence without suppressing any failure counts.
			for range maxCryptoFailures + 2 {
				observer.observe(encrypt(wrong, 1001))
			}
			report = observer.report()
			require.Len(t, report.Samples, maxCryptoFailures)
			require.EqualValues(t, 3, report.SamplesOmitted)
			require.Equal(t, maxCryptoFailures+3, report.Failures.AuthTag)
		})
	}
}

// A late pre-wrap packet authenticates with the older ROC. It must not move
// the recorded index forward, or every later sample is one ROC too high.
func TestCryptoDiagnosticLatePacketAcrossWrap(t *testing.T) {
	for _, profile := range []srtp.ProtectionProfile{srtp.ProtectionProfileAes128CmHmacSha1_80, srtp.ProtectionProfileAeadAes128Gcm} {
		t.Run(profile.String(), func(t *testing.T) {
			keyLen, err := profile.KeyLen()
			require.NoError(t, err)
			saltLen, err := profile.SaltLen()
			require.NoError(t, err)
			key, salt := make([]byte, keyLen), make([]byte, saltLen)
			observer := newCryptoObserver(time.Now())
			observer.profile, observer.keys = profile, srtp.SessionKeys{RemoteMasterKey: key, RemoteMasterSalt: salt}
			observer.receiver, err = srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(64))
			require.NoError(t, err)
			observer.stats.Ready = true
			sender, err := srtp.CreateContext(key, salt, profile)
			require.NoError(t, err)
			sent := map[uint16][]byte{}
			for _, seq := range []uint16{65530, 65531, 65532, 65533, 65534, 65535, 0, 1, 2, 3, 4} {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 42, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte("media")}).Marshal()
				require.NoError(t, err)
				sent[seq], err = sender.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
			}
			for _, seq := range []uint16{65530, 65531, 65532, 65533, 65535, 0, 1, 2, 3, 65534} {
				observer.observe(sent[seq])
			}
			require.EqualValues(t, 1<<16|3, observer.highest[42], "the late packet keeps its own ROC")
			corrupt := append([]byte(nil), sent[4]...)
			corrupt[len(corrupt)-1] ^= 0xff
			observer.observe(corrupt)
			report := observer.report()
			require.Equal(t, DecryptionFailures{AuthTag: 1}, report.Failures)
			require.Len(t, report.Samples, 1)
			require.EqualValues(t, 1<<16|3, report.Samples[0].PreviousIndex)
			require.EqualValues(t, 1<<16|4, report.Samples[0].EstimatedIndex)
			require.Nil(t, report.Samples[0].AuthenticatedIndex, "corruption is not a ROC mismatch")
		})
	}
}
