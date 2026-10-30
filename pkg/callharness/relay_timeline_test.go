package callharness

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/internal/iceufrag"
	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type relayTimelineEvent struct {
	at               time.Time
	name, path, kind string
	generation       uint64
	ssrc             uint32
	sequence         uint16
	nominate         bool
}
type relayTimeline struct {
	mu     sync.Mutex
	events []relayTimelineEvent
}

func (r *relayTimeline) observe(at time.Time, name, path string, raw []byte) {
	e := relayTimelineEvent{at: at, name: name, path: path}
	if len(raw) > 0 {
		if stun.IsMessage(raw) {
			m := &stun.Message{Raw: raw}
			if m.Decode() != nil {
				return
			}
			switch m.Type {
			case stun.BindingRequest:
				e.kind = "check"
				e.nominate = m.Contains(stun.AttrUseCandidate)
				var user stun.Username
				if user.GetFrom(m) == nil {
					local, _, _ := strings.Cut(user.String(), ":")
					_, e.generation, _ = iceufrag.Parse(local)
				}
			case stun.BindingSuccess:
				e.kind = "response"
			default:
				return
			}
		} else {
			if len(raw) < 12 || raw[0]>>6 != 2 || raw[1] >= 192 && raw[1] <= 223 {
				return
			}
			var h rtp.Header
			if _, err := h.Unmarshal(raw); err != nil {
				return
			}
			switch h.PayloadType {
			case 111:
				e.kind = kindAudio
			case 96:
				e.kind = kindVideo
			default:
				return
			}
			e.ssrc, e.sequence = h.SSRC, h.SequenceNumber
		}
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}
func (r *relayTimeline) snapshot() []relayTimelineEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]relayTimelineEvent(nil), r.events...)
}

type relayTimelineConn struct{ net.PacketConn }

func (c relayTimelineConn) WriteTo(p []byte, to net.Addr) (int, error) {
	n, e := c.PacketConn.WriteTo(p, to)
	if e == nil {
		relaytrace.Emit("caller-send", to.String(), p)
	}
	return n, e
}
func (c relayTimelineConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, from, e := c.PacketConn.ReadFrom(p)
	if e == nil {
		relaytrace.Emit("caller-receive", from.String(), p[:n])
	}
	return n, from, e
}

// TestRelayRestartTimeline measures the unchanged caller and server. It does not
// substitute server timing for the caller timing acceptance gate.
func TestRelayRestartTimeline(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		var trace relayTimeline
		t.Cleanup(relaytrace.Observe(trace.observe))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var relays []*relay.Relay
		var bindings []controlplane.RelayBinding
		for _, id := range []string{"r1", "r2"} {
			r, err := relay.New(relay.Config{Owners: store, SlotID: id})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			relays = append(relays, r)
			bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id, Leg: r.WorkerAddr(), Public: r.PublicAddr()}, Relay: r})
		}
		w, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: relays[0].WorkerAddr(), PublicAddr: relays[0].PublicAddr()}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		p := controlplane.New(relays[0], store)
		require.NoError(t, p.Register("0", w.LocalAddr(), w))
		for _, r := range relays {
			r.AddWorker(w.LocalAddr())
		}
		require.NoError(t, p.ConfigureRelaySet(ctx, bindings))
		server := httptest.NewServer(p.Handler())
		t.Cleanup(server.Close)
		h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: relays[0].PublicAddr(), CallerSocket: func() (net.PacketConn, error) {
			c, e := net.ListenPacket("udp4", "127.0.0.1:0")
			if e != nil {
				return nil, e
			}
			return relayTimelineConn{c}, nil
		}}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, h.Close()) })
		call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, err)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 5*time.Second) }()
		time.Sleep(time.Second)
		for _, track := range call.rec.report().Tracks {
			t.Logf("TIMELINE_BASELINE track=%s max_ms=%.3f", track.Kind, float64(track.MediaGap)/float64(time.Millisecond))
		}
		for i := range 2 {
			start := time.Now()
			target := 1 - i%2
			require.NoError(t, p.ChangeRelay(ctx, call.SessionID(), bindings[target].Slot.ID))
			require.Eventually(t, func() bool { return call.selectedRemoteAddr() == relays[target].PublicAddr().String() }, time.Second, time.Millisecond)
			time.Sleep(650 * time.Millisecond)
			timelineChange(t, trace.snapshot(), call, start, bindings[1-target].Slot, bindings[target].Slot, uint64(i+1))
		}
		require.NoError(t, <-sent)
		failures, err := w.SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		for _, track := range report.Tracks {
			require.Zero(t, track.DuplicatePackets)
		}
	})
}

func timelineChange(t *testing.T, events []relayTimelineEvent, call *Call, start time.Time, old, next mediaworker.RelaySlot, generation uint64) {
	t.Helper()
	find := func(name, path, kind string, after time.Time, last bool) relayTimelineEvent {
		var found relayTimelineEvent
		for _, e := range events {
			if e.at.Before(after) || e.name != name || path != "*" && e.path != path || kind != "" && e.kind != kind {
				continue
			}
			if (kind == "check") && e.generation != generation {
				continue
			}
			if found.at.IsZero() || last && e.at.After(found.at) || !last && e.at.Before(found.at) {
				found = e
			}
		}
		require.False(t, found.at.IsZero(), "missing %s %s %s", name, path, kind)
		return found
	}
	event := find("event-sent", "*", "", start, false)
	log := func(label string, e relayTimelineEvent) {
		t.Logf("TIMELINE gen=%d event=%s ms=%.3f path=%q ssrc=%d sequence=%d nominate=%t", generation, label, float64(e.at.Sub(event.at))/float64(time.Millisecond), e.path, e.ssrc, e.sequence, e.nominate)
	}
	log("event-sent", event)
	for _, name := range []string{"offer-start-pair", "offer-created-pair", "offer-post", "worker-answer", "answer-applied"} {
		log(name, find(name, "*", "", start, false))
	}
	log("first-check-target", find("relay-input", next.ID, "check", start, false))
	log("first-check-worker", find("worker-check", next.Leg.String(), "check", start, false))
	log("first-binding-response-target", find("relay-output", next.ID, "response", start, false))
	log("use-candidate", find("use-candidate", next.Leg.String(), "", start, false))
	switched := find("worker-switch", next.Leg.String(), "", start, false)
	log("worker-switch", switched)
	require.True(t, switched.at.Before(find("use-candidate", next.Leg.String(), "", start, false).at), "Pion switches on authenticated media before nomination")
	log("keyframe-request", find("keyframe-request", next.Leg.String(), "", start, false))
	for _, kind := range []string{kindAudio, kindVideo} {
		for _, boundary := range []struct{ name, from, to string }{
			{"caller-send", old.Public.String(), next.Public.String()},
			{"relay-input", old.ID, next.ID},
			{"worker-arrival", old.Leg.String(), next.Leg.String()},
			{"worker-input", old.Leg.String(), next.Leg.String()},
			{"worker-output", old.Leg.String(), next.Leg.String()},
			{"relay-output", old.ID, next.ID},
			{"caller-receive", old.Public.String(), next.Public.String()},
		} {
			last := find(boundary.name, boundary.from, kind, start.Add(-300*time.Millisecond), true)
			first := find(boundary.name, boundary.to, kind, start, false)
			log(fmt.Sprintf("last-%s-%s-old", boundary.name, kind), last)
			log(fmt.Sprintf("first-%s-%s-new", boundary.name, kind), first)
			t.Logf("TIMELINE_GAP gen=%d boundary=%s track=%s ms=%.3f", generation, boundary.name, kind, float64(first.at.Sub(last.at))/float64(time.Millisecond))
		}
	}
	for _, kind := range []string{kindAudio, kindVideo} {
		early, rejected, wrongOutput := 0, 0, 0
		for _, e := range events {
			if e.at.Before(start) || !e.at.Before(switched.at) || e.path != next.Leg.String() || e.kind != kind {
				continue
			}
			switch e.name {
			case "worker-arrival":
				early++
			case "worker-reject":
				rejected++
			case "worker-output":
				wrongOutput++
			}
		}
		t.Logf("TIMELINE_EARLY gen=%d track=%s pending_arrivals=%d pending_rejected=%d pending_output=%d", generation, kind, early, rejected, wrongOutput)
		require.Zero(t, wrongOutput, "pending relay must not carry worker output")
	}
	call.rec.mu.Lock()
	defer call.rec.mu.Unlock()
	var decoded time.Time
	for _, track := range call.rec.tracks {
		var last, first time.Time
		for _, arrival := range track.arrivals {
			at := call.rec.start.Add(arrival)
			if at.Before(switched.at) {
				last = at
			} else if at.After(switched.at) && first.IsZero() {
				first = at
			}
		}
		require.False(t, last.IsZero())
		require.False(t, first.IsZero())
		log("last-caller-application-"+track.kind, relayTimelineEvent{at: last})
		log("first-caller-application-"+track.kind, relayTimelineEvent{at: first})
		t.Logf("TIMELINE_GAP gen=%d boundary=caller-application track=%s ms=%.3f", generation, track.kind, float64(first.Sub(last))/float64(time.Millisecond))
		require.LessOrEqual(t, first.Sub(last), relayTimingLimit(150*time.Millisecond))
		delivered := make(map[uint16]bool, len(track.headers))
		for _, h := range track.headers {
			delivered[h.seq] = true
		}
		wire, app := 0, 0
		for _, e := range events {
			if e.name == "caller-receive" && e.path == old.Public.String() && e.kind == track.kind && e.ssrc == track.ssrc && e.at.After(event.at) && e.at.Before(switched.at) {
				wire++
				if delivered[e.sequence] {
					app++
				}
			}
		}
		t.Logf("TIMELINE_OLD_PAIR gen=%d track=%s udp_received=%d application_received=%d", generation, track.kind, wire, app)
		if track.video == nil {
			continue
		}
		for _, f := range track.video.frames {
			at := call.rec.start.Add(f.at)
			if f.decodable && at.After(switched.at) && (decoded.IsZero() || at.Before(decoded)) {
				decoded = at
			}
		}
	}
	require.False(t, decoded.IsZero(), "missing decodable frame after switch")
	log("first-decodable-frame", relayTimelineEvent{at: decoded})
}
