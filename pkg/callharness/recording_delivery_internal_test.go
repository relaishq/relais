package callharness

import (
	"strconv"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// Rolling history drops sent units after 15 s; the whole-call counts must not.
func TestWholeCallDeliverySurvivesRollingHistory(t *testing.T) {
	r := newRecorder()
	r.recording.History = 15 * time.Second
	audio := r.addTrack(kindAudio, 1, 111)
	const units, late = 3000, 2000
	tick := 20 * time.Millisecond
	lost := map[int]bool{100: true, 101: true, 1500: true, 2990: true}
	var latePacket []byte
	var seq uint16
	for i := range units {
		at := time.Second + time.Duration(i)*tick
		packet, err := r.sendingAudio([]byte{0x78, 1, 2}, tick)
		require.NoError(t, err)
		u := r.sentAudioUnits[string(packet)]
		u.at = at
		r.sentAudioUnits[string(packet)] = u
		r.sent(kindAudio, false)
		switch {
		case i == late:
			latePacket = packet
		case !lost[i]:
			r.packet(audio, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: packet}, r.start.Add(at+5*time.Millisecond))
			seq++
		}
		// Three seconds late, but inside history: late content is not lost.
		if i == late+150 {
			r.packet(audio, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: latePacket}, r.start.Add(at+6*time.Millisecond))
			seq++
		}
		r.compact(at + 7*time.Millisecond)
	}
	r.hungUpAt = time.Second + units*tick
	running, retained := r.audioDelivery.Sent, len(r.sentAudioUnits)
	rep := r.report()
	require.LessOrEqual(t, rep.Recording.ContentUnits, 800, "history stays bounded")
	d := rep.Recording.Delivery
	require.NotNil(t, d)
	// Units sent less than 500ms before hangup are unjudged, even lost unit 2990.
	require.Equal(t, 24, d.Audio.Unsettled)
	require.Equal(t, units, d.Audio.Sent+d.Audio.Unsettled)
	require.Equal(t, rep.SentAudio.Frames, d.Audio.Sent+d.Audio.Unsettled)
	require.Equal(t, 3, d.Audio.Lost)
	require.Equal(t, d.Audio.Sent-3, d.Audio.Returned)
	require.Equal(t, []time.Duration{3 * time.Second, 3*time.Second + tick, 31 * time.Second}, d.Audio.LostAt)
	require.Zero(t, d.Audio.LostOverflow)
	require.Zero(t, d.Video.Sent+d.Video.Unsettled)

	// The final report settles a copy. Running counts and history are unchanged.
	require.Less(t, running, d.Audio.Sent)
	require.Equal(t, running, r.audioDelivery.Sent)
	require.Len(t, r.sentAudioUnits, retained)
}

func TestWholeCallDeliveryCountsVideoFramesOnce(t *testing.T) {
	r := newRecorder()
	r.recording.History = 2 * time.Second
	interval := 33 * time.Millisecond
	for i := range 600 {
		at := time.Second + time.Duration(i)*interval
		f := sentVideoFrame{identity: uint64(i), at: at, written: i != 599, returnedAt: at + 10*time.Millisecond}
		switch i {
		case 50, 400:
			f.returnedAt = 0
		case 300:
			f.returnedAt = at - time.Millisecond // not a valid return of this send
		}
		r.sentVideoFrames[uint16(i)] = f
		r.compact(at + time.Millisecond)
	}
	r.hungUpAt = time.Second + 600*interval
	d := r.report().Recording.Delivery
	require.Equal(t, 14, d.Video.Unsettled)
	require.Equal(t, 585, d.Video.Sent, "an unwritten frame is not content")
	require.Equal(t, 3, d.Video.Lost)
	require.Equal(t, 582, d.Video.Returned)
	require.Equal(t, []time.Duration{time.Second + 50*interval, time.Second + 300*interval, time.Second + 400*interval}, d.Video.LostAt)
}

func TestWholeCallDeliveryLostInventoryIsBounded(t *testing.T) {
	r := newRecorder()
	r.recording.History = 15 * time.Second
	for i := range 5000 {
		r.sentAudioUnits[strconv.Itoa(i)] = contentUnit{at: time.Second + time.Duration(i)*20*time.Millisecond, written: true}
	}
	r.compact(200 * time.Second)
	require.Empty(t, r.sentAudioUnits)
	r.hungUpAt = 200 * time.Second
	d := r.report().Recording.Delivery
	require.Equal(t, 5000, d.Audio.Lost)
	require.Len(t, d.Audio.LostAt, MaxRetainedLostUnits)
	require.Equal(t, 5000-MaxRetainedLostUnits, d.Audio.LostOverflow)
	require.IsNonDecreasing(t, d.Audio.LostAt)
}
