package mediaworker

import (
	"encoding/json"
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

func TestUnsentTrackRetainsPreWrapRunway(t *testing.T) {
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			keys := testSessionKeys(t, profile)
			track := trackState{MID: "video", SSRC: 123, InitialSeq: 30000}
			state := sessionState{Video: track}
			attempts, err := state.sequenceResumeAttempts(8192)
			require.NoError(t, err)
			require.Equal(t, 3, attempts, "three margins preserve the lost-start runway")
			for step := range 3 {
				out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				sess := &session{srtpOut: out}
				require.NoError(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}))
				fresh := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				_, err := fresh.DecryptRTP(nil, testEncrypt(t, out, track.SSRC, track.InitialSeq), nil)
				require.NoError(t, err, "first ever packet at step %d", step+1)
				encoded, err := json.Marshal(track)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(encoded, &track))
			}
			before := track
			sess := &session{srtpOut: testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)}
			require.ErrorIs(t, sess.resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}), ErrSequenceBudgetExhausted)
			require.Equal(t, before, track)
			// A sent sibling still constrains the session to the half-space budget.
			state.Audio = trackState{MID: "audio", Packets: 1}
			attempts, err = state.sequenceResumeAttempts(8192)
			require.NoError(t, err)
			require.Equal(t, 2, attempts)
		})
	}
}

func TestSequenceBudgetReservesFirstResumedPacket(t *testing.T) {
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			keys := testSessionKeys(t, profile)
			const ssrc = 123
			for _, margin := range []uint16{22766, 22767} {
				old := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				_, err := receiver.DecryptRTP(nil, testEncrypt(t, old, ssrc, 60000), nil)
				require.NoError(t, err)
				out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				track := trackState{MID: "video", SSRC: ssrc, Packets: 1, HighestSentIndex: 60000}
				before := track
				err = (&session{srtpOut: out}).resumeTrack(&track, ResumeOptions{SequenceMargin: margin})
				if margin == 22767 {
					require.ErrorIs(t, err, ErrSequenceBudgetExhausted)
					require.Equal(t, before, track)
					// Independently demonstrate why this rejected step is unsafe: restore
					// its counter as the old guard allowed, then use the full outage gap
					// plus the first resumed packet. Exactly half-space is interpreted as old ROC-zero traffic.
					require.NoError(t, restoreOutboundIndex(out, ssrc, 60000+uint64(margin)))
				} else {
					require.NoError(t, err)
				}
				seq := uint16(60000 + uint64(margin) + SequenceGapReserve + 1) //nolint:gosec // low 16 bits of the test index
				_, err = receiver.DecryptRTP(nil, testEncrypt(t, out, ssrc, seq), nil)
				if margin == 22767 {
					require.Error(t, err, "exact half-space fails decryption")
				} else {
					require.NoError(t, err)
				}
			}
		})
	}
}

func TestSRTCPRestoreAtIndexBoundary(t *testing.T) {
	const maximum = uint32(1<<31 - 1)
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			keys := testSessionKeys(t, profile)
			const ssrc = 123
			raw, err := (&rtcp.PictureLossIndication{SenderSSRC: ssrc, MediaSSRC: 456}).Marshal()
			require.NoError(t, err)
			receiver, err := srtp.CreateContext(keys.LocalMasterKey, keys.LocalMasterSalt, profile, srtp.SRTCPReplayProtection(replayWindow))
			require.NoError(t, err)
			old := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			old.SetIndex(ssrc, maximum-3)
			previous, err := old.EncryptRTCP(nil, raw, nil)
			require.NoError(t, err)
			_, err = receiver.DecryptRTCP(nil, previous, nil)
			require.NoError(t, err)
			track := trackState{MID: "video", SSRC: ssrc, Packets: 1, HighestSentIndex: 1000, SRTCPIndex: maximum - 2}
			out := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			require.NoError(t, (&session{srtpOut: out}).resumeTrack(&track, ResumeOptions{SRTCPIndexMargin: 1}))
			encrypted, err := out.EncryptRTCP(nil, raw, nil)
			require.NoError(t, err)
			_, err = receiver.DecryptRTCP(nil, encrypted, nil)
			require.NoError(t, err)
			index, ok := out.Index(ssrc)
			require.True(t, ok)
			require.Equal(t, maximum, index)
			track.SRTCPIndex = index
			restored := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			require.NoError(t, (&session{srtpOut: restored}).resumeTrack(&track, ResumeOptions{}))
			_, err = restored.EncryptRTCP(nil, raw, nil)
			require.Error(t, err, "Pion rejects key exhaustion rather than reusing index zero")
			for _, packets := range []uint64{0, 1} {
				track.Packets = packets
				before := track
				require.ErrorIs(t, (&session{srtpOut: restored}).resumeTrack(&track, ResumeOptions{SRTCPIndexMargin: 1}), ErrSRTCPIndexExhausted,
					"a margin must not bypass Pion's key-exhaustion guard via SetIndex modulo")
				require.Equal(t, before, track)
			}
			t.Logf("SRTCP_BOUNDARY profile=%s final_decrypt_index=%d exhaustion_rejected=true margin_wrap_rejected=true", profileName(profile), index)
		})
	}
}

// Drop a full runway of initial packets at the caller, with either no prior
// index or only low stale indexes from before the unsent snapshot's retries.
func TestUnsentTrackLostStartDecryptsAfterWrap(t *testing.T) {
	for _, profile := range testSRTPProfiles {
		t.Run(profileName(profile), func(t *testing.T) {
			for _, initial := range []uint16{30000, 32767} {
				for _, stale := range []bool{false, true} {
					keys := testSessionKeys(t, profile)
					receiver := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
					if stale {
						old := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
						for seq := initial; seq < initial+3 && seq <= 1<<15; seq++ {
							_, err := receiver.DecryptRTP(nil, testEncrypt(t, old, 123, seq), nil)
							require.NoError(t, err)
						}
					}
					track := trackState{MID: "video", SSRC: 123, InitialSeq: initial}
					var out *srtp.Context
					for range 3 {
						out = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
						require.NoError(t, (&session{srtpOut: out}).resumeTrack(&track, ResumeOptions{SequenceMargin: 8192}))
					}
					decryptedAfterWrap := 0
					for index := uint32(track.InitialSeq); index < 65536+2000; index++ {
						encrypted := testEncrypt(t, out, track.SSRC, uint16(index)) //nolint:gosec // low 16 bits of an SRTP index
						if index < uint32(track.InitialSeq)+unsentRunway {
							continue
						}
						_, err := receiver.DecryptRTP(nil, encrypted, nil)
						require.NoError(t, err, "initial=%d stale=%t index=%d", initial, stale, index)
						if index >= 65536 {
							decryptedAfterWrap++
						}
					}
					require.Equal(t, 2000, decryptedAfterWrap)
				}
			}
		})
	}
}
