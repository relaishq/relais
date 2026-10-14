package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relais/internal/loadgen"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func soakDeliveryCall(r *loadgen.RunReport, call int, audio, video callharness.ContentDelivery) {
	report := r.Processes[0].Calls[call].Call
	report.SentAudio.Frames = audio.Sent + audio.Unsettled
	report.SentVideo.Frames = video.Sent + video.Unsettled
	report.Recording.Delivery = &callharness.WholeCallDelivery{Audio: audio, Video: video}
}

func soakHasCause(v soakReport, prefix string) bool {
	for _, cause := range v.Causes {
		if strings.HasPrefix(cause, prefix) {
			return true
		}
	}
	return false
}

func TestSoakNoLossCallsPassWholeCallDelivery(t *testing.T) {
	c, r := soakFixture()
	soakDeliveryCall(&r, 0, callharness.ContentDelivery{Sent: 59990, Returned: 59990, Unsettled: 10}, callharness.ContentDelivery{Sent: 35994, Returned: 35994, Unsettled: 6})
	soakDeliveryCall(&r, 1, callharness.ContentDelivery{Sent: 60000, Returned: 60000}, callharness.ContentDelivery{Sent: 36000, Returned: 36000})
	r.Processes[0].Calls[1].Call.Tracks = []callharness.TrackReport{{Kind: "audio", SequenceDiscontinuities: 3, MediaGapsRecorded: true}}
	v := judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome, v.Causes)
	require.Equal(t, "pass", v.Delivery.Outcome)
	require.Len(t, v.Delivery.Calls, 2)
	require.Equal(t, 119990, v.Delivery.Audio.Sent)
	require.Equal(t, 16, v.Delivery.Audio.Unsettled+v.Delivery.Video.Unsettled)
	require.Zero(t, v.Delivery.Audio.Lost+v.Delivery.Video.Lost)
	require.Equal(t, 3, v.Delivery.Audio.SequenceDiscontinuities, "discontinuities are reported, never judged")
}

// The #127 Linux soak: calls never moved still lost echoed content, and no
// event window covered it.
func TestSoakWholeCallLossOutsideEventWindowsFails(t *testing.T) {
	for _, tc := range []struct {
		name         string
		saturatedPID int
		want         string
	}{{"uncontended", 0, "fail"}, {"own generator saturated", 42, "inconclusive"}, {"other generator saturated", 43, "fail"}} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			soakDeliveryCall(&r, 0, callharness.ContentDelivery{Sent: 1000, Returned: 1000}, callharness.ContentDelivery{})
			lost := []time.Duration{599500 * time.Millisecond, 599520 * time.Millisecond}
			soakDeliveryCall(&r, 1, callharness.ContentDelivery{Sent: 50000, Returned: 49998, Lost: 2, LostAt: lost}, callharness.ContentDelivery{Sent: 30000, Returned: 29999, Lost: 1, LostAt: []time.Duration{599510 * time.Millisecond}})
			if tc.saturatedPID != 0 {
				sample := loadgen.Stats{At: r.StartedAt.Add(600 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: 41 * time.Millisecond}}
				if tc.saturatedPID == 42 {
					r.Processes[0].Samples[600] = sample
					r.Processes[0].Lateness.Max = 41 * time.Millisecond
				} else {
					r.Processes = append(r.Processes, loadgen.ProcessReport{PID: 43, Complete: true, Samples: []loadgen.Stats{sample}})
				}
			}
			v := judgeSoak(c, r)
			require.Equal(t, tc.want, v.Outcome, v.Causes)
			require.Equal(t, tc.want, v.Delivery.Outcome)
			call := v.Delivery.Calls[1]
			require.Equal(t, "b", call.Session)
			require.Equal(t, 2, call.Audio.LostOutside)
			require.Equal(t, 1, call.Video.LostOutside)
			require.Zero(t, call.Audio.LostInEvents)
			require.Equal(t, r.StartedAt.Add(lost[0]), call.FirstOutsideLossAt)
			require.Equal(t, 50000+1000, v.Delivery.Audio.Sent)
			require.Equal(t, 2, v.Delivery.Audio.Lost)
			if tc.want == "fail" {
				require.Zero(t, call.Audio.LostOutsideSaturated)
				require.Equal(t, 1, v.Delivery.FailedCalls)
				require.Contains(t, v.Causes, "whole-call content loss outside event windows in 1/2 calls: 2 audio units, 1 video frames, 0 unplaced")
			} else {
				require.Equal(t, 2, call.Audio.LostOutsideSaturated)
				require.Equal(t, 1, call.Video.LostOutsideSaturated)
				require.Contains(t, v.Causes, "whole-call delivery inconclusive in 1/2 calls")
				require.False(t, soakHasCause(v, "whole-call content loss"))
			}
		})
	}
}

// A lost unit in an affected call's event window belongs to that event's
// content-loss row only. The same unit just after the window is steady state.
func TestSoakEventWindowLossIsJudgedOnce(t *testing.T) {
	c, r := soakFixture()
	move := &r.Processes[0].Calls[0].Call.Moves[0]
	move.Measurement.WindowStart, move.Measurement.WindowEnd = 30*time.Second-250*time.Millisecond, 31*time.Second+250*time.Millisecond
	move.Measurement.AudioLoss = callharness.MetricVerdict{Trusted: true, Failed: true}
	move.Measurement.LostAudio = 20 * time.Millisecond
	soakDeliveryCall(&r, 0, callharness.ContentDelivery{Sent: 1000, Returned: 999, Lost: 1, LostAt: []time.Duration{30500 * time.Millisecond}}, callharness.ContentDelivery{})
	r.Judge()
	v := judgeSoak(c, r)
	require.Equal(t, "fail", v.Outcome)
	require.Equal(t, "fail", v.Events[0].Outcome, "the event row judges its own window")
	require.Contains(t, v.Causes, "event 0 failed caller recovery rows")
	call := v.Delivery.Calls[0]
	require.Equal(t, 3, call.EventWindows)
	require.Equal(t, 1, call.Audio.LostInEvents)
	require.Zero(t, call.Audio.LostOutside)
	require.Equal(t, "pass", call.Outcome)
	require.Equal(t, "pass", v.Delivery.Outcome)
	require.False(t, soakHasCause(v, "whole-call"), "no lost unit is judged twice")

	// WindowEnd is exclusive, exactly as the caller's missingInWindow.
	move.Measurement.AudioLoss.Failed, move.Measurement.LostAudio = false, 0
	r.Processes[0].Calls[0].Call.Recording.Delivery.Audio.LostAt = []time.Duration{move.Measurement.WindowEnd}
	r.Judge()
	v = judgeSoak(c, r)
	require.Equal(t, "pass", v.Events[0].Outcome)
	require.Equal(t, 1, v.Delivery.Calls[0].Audio.LostOutside)
	require.Equal(t, "fail", v.Outcome)
	require.Contains(t, v.Causes, "whole-call content loss outside event windows in 1/2 calls: 1 audio units, 0 video frames, 0 unplaced")
}

func TestSoakWholeCallDeliveryNeedsCompleteEvidence(t *testing.T) {
	c, r := soakFixture()
	r.Processes[0].Calls[1].Call.Recording.Delivery = nil
	v := judgeSoak(c, r)
	require.Equal(t, "inconclusive", v.Outcome, "old reports lack whole-call delivery")
	require.Equal(t, []string{"whole-call delivery unavailable"}, v.Delivery.Calls[1].Causes)

	c, r = soakFixture()
	soakDeliveryCall(&r, 1, callharness.ContentDelivery{Sent: 9000, Returned: 4000, Lost: 5000, LostAt: make([]time.Duration, callharness.MaxRetainedLostUnits), LostOverflow: 5000 - callharness.MaxRetainedLostUnits}, callharness.ContentDelivery{})
	for i := range r.Processes[0].Calls[1].Call.Recording.Delivery.Audio.LostAt {
		r.Processes[0].Calls[1].Call.Recording.Delivery.Audio.LostAt[i] = time.Duration(i+1) * 20 * time.Millisecond
	}
	v = judgeSoak(c, r)
	require.Equal(t, "fail", v.Outcome)
	require.Equal(t, 904, v.Delivery.Calls[1].Audio.LostOverflow)
	require.Contains(t, v.Delivery.Calls[1].Causes, "audio lost-unit inventory overflow: 904 lost units have no send time")

	c, r = soakFixture()
	soakDeliveryCall(&r, 1, callharness.ContentDelivery{Sent: 100, Returned: 100}, callharness.ContentDelivery{})
	r.Processes[0].Calls[1].Call.SentAudio.Frames = 101
	v = judgeSoak(c, r)
	require.Equal(t, "inconclusive", v.Outcome)
	require.Contains(t, v.Delivery.Calls[1].Causes, "audio whole-call accounting covers 100 of 101 written units")
}

const procNetSNMP = `Ip: Forwarding DefaultTTL InReceives InHdrErrors InAddrErrors ForwDatagrams InUnknownProtos InDiscards InDelivers OutRequests OutDiscards OutNoRoutes ReasmTimeout ReasmReqds ReasmOKs ReasmFails FragOKs FragFails FragCreates OutTransmits
Ip: 1 64 1868139 0 0 0 0 0 1868013 1683870 0 0 0 0 0 0 0 0 0 1683870
Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
Udp: 91246 2 35 91010 %d 0 0 0 0
UdpLite: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
UdpLite: 0 0 0 0 999 0 0 0 0
`

const procNetSNMP6 = `Udp6InDatagrams                 	120
Udp6NoPorts                     	0
Udp6InErrors                    	%d
Udp6OutDatagrams                	120
Udp6RcvbufErrors                	%d
UdpLite6RcvbufErrors            	999
`

func writeProcNet(t *testing.T, snmp string, snmp6 *string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snmp"), []byte(snmp), 0o600))
	if snmp6 != nil {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "snmp6"), []byte(*snmp6), 0o600))
	}
	return dir
}

func TestSoakKernelUDPCounterFixture(t *testing.T) {
	sprintf := fmt.Sprintf
	start6, end6 := sprintf(procNetSNMP6, 0, 0), sprintf(procNetSNMP6, 7, 7)
	start, err := readKernelUDP(writeProcNet(t, sprintf(procNetSNMP, 35), &start6))
	require.NoError(t, err)
	require.Equal(t, kernelUDPCounters{RcvbufErrors: 35, UDP6: true}, start, "UdpLite counters are not UDP")
	end, err := readKernelUDP(writeProcNet(t, sprintf(procNetSNMP, 1168), &end6))
	require.NoError(t, err)
	d := kernelUDPDelta("linux", "loopback", start, end, nil, nil)
	require.True(t, d.Available)
	require.Equal(t, uint64(1133), d.RcvbufErrors)
	require.True(t, d.UDP6)
	require.Equal(t, uint64(7), d.UDP6RcvbufErrors)
	require.Contains(t, d.Scope, "every loopback role")
	require.Contains(t, kernelUDPDelta("linux", "netns", start, end, nil, nil).Scope, "netns roles are not included")

	// A kernel without IPv6 has no snmp6; IPv4 remains available.
	noV6, err := readKernelUDP(writeProcNet(t, sprintf(procNetSNMP, 4), nil))
	require.NoError(t, err)
	require.False(t, noV6.UDP6)
	d = kernelUDPDelta("linux", "loopback", noV6, end, nil, nil)
	require.True(t, d.Available)
	require.False(t, d.UDP6)

	_, err = readKernelUDP(writeProcNet(t, "Udp: InDatagrams RcvbufErrors\nUdp: 1\n", nil))
	require.Error(t, err)
	_, err = readKernelUDP(writeProcNet(t, "Ip: Forwarding\nIp: 1\n", nil))
	require.Error(t, err)
	_, err = readKernelUDP(t.TempDir())
	require.Error(t, err)

	d = kernelUDPDelta("darwin", "loopback", kernelUDPCounters{}, kernelUDPCounters{}, errKernelUDPUnavailable, errKernelUDPUnavailable)
	require.False(t, d.Available)
	require.Equal(t, "not available on darwin", d.Reason)
	d = kernelUDPDelta("linux", "loopback", end, start, nil, nil)
	require.False(t, d.Available, "a decreasing counter is not a measurement")
}
