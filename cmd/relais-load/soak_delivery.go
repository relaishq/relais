package main

import (
	"fmt"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

// soakDeliverySettle matches the caller yardstick's unexported 500 ms content
// settle window: a valid return is expected within it.
const soakDeliverySettle = 500 * time.Millisecond

const soakDeliveryRule = "every written audio unit and video frame must return; a lost unit inside one of its call's matched event windows is judged only by that event; any other lost unit fails, unless its send time plus the 500ms settle window overlaps a saturated interval of its own generator (inconclusive)"

// deliveryCounts is one kind's whole-call delivery. LostInEvents, LostOutside
// and LostOverflow partition Lost. LostOutsideSaturated is the part of
// LostOutside excused as inconclusive. Discontinuities are diagnostic only:
// replays and planned resumes also break the echoed sequence.
type deliveryCounts struct {
	Sent                    int `json:"sent"`
	Returned                int `json:"returned"`
	Lost                    int `json:"lost"`
	Unsettled               int `json:"unsettled"`
	LostInEvents            int `json:"lost_in_event_windows"`
	LostOutside             int `json:"lost_outside_event_windows"`
	LostOutsideSaturated    int `json:"lost_outside_saturated"`
	LostOverflow            int `json:"lost_inventory_overflow"`
	SequenceDiscontinuities int `json:"sequence_discontinuities"`
}

func (c *deliveryCounts) add(o deliveryCounts) {
	c.Sent += o.Sent
	c.Returned += o.Returned
	c.Lost += o.Lost
	c.Unsettled += o.Unsettled
	c.LostInEvents += o.LostInEvents
	c.LostOutside += o.LostOutside
	c.LostOutsideSaturated += o.LostOutsideSaturated
	c.LostOverflow += o.LostOverflow
	c.SequenceDiscontinuities += o.SequenceDiscontinuities
}

type callDelivery struct {
	PID                int            `json:"pid"`
	CallID             int            `json:"call_id"`
	Session            string         `json:"session"`
	EventWindows       int            `json:"event_windows"`
	Audio              deliveryCounts `json:"audio"`
	Video              deliveryCounts `json:"video"`
	FirstOutsideLossAt time.Time      `json:"first_outside_loss_at,omitzero"`
	Outcome            string         `json:"outcome"`
	Causes             []string       `json:"causes,omitempty"`
}

// soakDelivery is whole-call content delivery for every reported call, and
// run totals. Its outcome joins the soak verdict.
type soakDelivery struct {
	Rule              string         `json:"rule"`
	Outcome           string         `json:"outcome"`
	FailedCalls       int            `json:"failed_calls"`
	InconclusiveCalls int            `json:"inconclusive_calls"`
	Audio             deliveryCounts `json:"audio"`
	Video             deliveryCounts `json:"video"`
	Calls             []callDelivery `json:"calls"`
}

func judgeDelivery(r loadgen.RunReport, b saturationBudget) soakDelivery {
	d := soakDelivery{Rule: soakDeliveryRule, Outcome: "pass"}
	for _, p := range r.Processes {
		for _, m := range p.Calls {
			if m.Call == nil {
				continue // Missing caller reports are judged elsewhere.
			}
			c := judgeCallDelivery(r.Events, p.PID, m, b)
			d.Audio.add(c.Audio)
			d.Video.add(c.Video)
			switch c.Outcome {
			case "fail":
				d.FailedCalls++
				d.Outcome = "fail"
			case "inconclusive":
				d.InconclusiveCalls++
				if d.Outcome != "fail" {
					d.Outcome = "inconclusive"
				}
			}
			d.Calls = append(d.Calls, c)
		}
	}
	return d
}

// judgeCallDelivery uses the exact windows the event yardstick judges for this
// call: [WindowStart, WindowEnd) of each move that matches an event affecting
// it. Loss there stays with the event, so no lost unit is judged twice.
func judgeCallDelivery(events []loadgen.EventResult, pid int, m loadgen.Message, b saturationBudget) callDelivery {
	call := m.Call
	c := callDelivery{PID: pid, CallID: m.CallID, Session: m.Session, Outcome: "pass"}
	if t := call.Track("audio"); t != nil {
		c.Audio.SequenceDiscontinuities = t.SequenceDiscontinuities
	}
	if t := call.Track("video"); t != nil {
		c.Video.SequenceDiscontinuities = t.SequenceDiscontinuities
	}
	var windows [][2]time.Duration
	for _, e := range events {
		if !slices.Contains(e.Sessions, m.Session) {
			continue
		}
		for _, move := range call.Moves {
			if loadgen.MatchesEvent(e, call, move) {
				windows = append(windows, [2]time.Duration{move.Measurement.WindowStart, move.Measurement.WindowEnd})
			}
		}
	}
	c.EventWindows = len(windows)
	fail := func(reason string) { c.Outcome = "fail"; c.Causes = append(c.Causes, reason) }
	inconclusive := func(reason string) {
		if c.Outcome != "fail" {
			c.Outcome = "inconclusive"
		}
		c.Causes = append(c.Causes, reason)
	}
	d := call.Recording.Delivery
	if d == nil {
		inconclusive("whole-call delivery unavailable")
		return c
	}
	for _, k := range []struct {
		kind    string
		in      callharness.ContentDelivery
		written int
		out     *deliveryCounts
	}{{"audio", d.Audio, call.SentAudio.Frames, &c.Audio}, {"video", d.Video, call.SentVideo.Frames, &c.Video}} {
		out := k.out
		out.Sent, out.Returned, out.Lost, out.Unsettled, out.LostOverflow = k.in.Sent, k.in.Returned, k.in.Lost, k.in.Unsettled, k.in.LostOverflow
		if k.in.Sent+k.in.Unsettled != k.written {
			inconclusive(fmt.Sprintf("%s whole-call accounting covers %d of %d written units", k.kind, k.in.Sent+k.in.Unsettled, k.written))
		}
		if k.in.LostOverflow > 0 {
			fail(fmt.Sprintf("%s lost-unit inventory overflow: %d lost units have no send time", k.kind, k.in.LostOverflow))
		}
		for _, at := range k.in.LostAt {
			if insideWindow(windows, at) {
				out.LostInEvents++
				continue
			}
			out.LostOutside++
			sent := call.StartedAt.Add(at)
			if c.FirstOutsideLossAt.IsZero() || sent.Before(c.FirstOutsideLossAt) {
				c.FirstOutsideLossAt = sent
			}
			for _, s := range b.Excluded {
				if s.PID == pid && s.Start.Before(sent.Add(soakDeliverySettle)) && s.End.After(sent) {
					out.LostOutsideSaturated++
					break
				}
			}
		}
		if n := out.LostOutside - out.LostOutsideSaturated; n > 0 {
			fail(fmt.Sprintf("%s lost %d units outside event windows", k.kind, n))
		}
		if out.LostOutsideSaturated > 0 {
			inconclusive(fmt.Sprintf("%s lost %d units outside event windows during this caller's generator saturation", k.kind, out.LostOutsideSaturated))
		}
	}
	return c
}

func insideWindow(windows [][2]time.Duration, at time.Duration) bool {
	for _, w := range windows {
		if at >= w[0] && at < w[1] {
			return true
		}
	}
	return false
}

// causes returns the soak-level causes; per-call detail stays in Calls.
func (d soakDelivery) causes() (failures, unknown []string) {
	total := len(d.Calls)
	if d.FailedCalls > 0 {
		failures = append(failures, fmt.Sprintf("whole-call content loss outside event windows in %d/%d calls: %d audio units, %d video frames, %d unplaced", d.FailedCalls, total, d.Audio.LostOutside-d.Audio.LostOutsideSaturated, d.Video.LostOutside-d.Video.LostOutsideSaturated, d.Audio.LostOverflow+d.Video.LostOverflow))
	}
	if d.InconclusiveCalls > 0 {
		unknown = append(unknown, fmt.Sprintf("whole-call delivery inconclusive in %d/%d calls", d.InconclusiveCalls, total))
	}
	return failures, unknown
}

func printSoakDelivery(d soakDelivery, k kernelUDPDrops) {
	fmt.Printf("soak delivery outcome=%s calls=%d failed-calls=%d inconclusive-calls=%d audio-sent=%d audio-lost=%d audio-lost-in-events=%d audio-lost-outside=%d video-sent=%d video-lost=%d video-lost-in-events=%d video-lost-outside=%d unsettled=%d seq-discontinuities=%d\n",
		d.Outcome, len(d.Calls), d.FailedCalls, d.InconclusiveCalls, d.Audio.Sent, d.Audio.Lost, d.Audio.LostInEvents, d.Audio.LostOutside, d.Video.Sent, d.Video.Lost, d.Video.LostInEvents, d.Video.LostOutside, d.Audio.Unsettled+d.Video.Unsettled, d.Audio.SequenceDiscontinuities+d.Video.SequenceDiscontinuities)
	for _, c := range d.Calls {
		for _, cause := range c.Causes {
			fmt.Printf("soak delivery pid=%d call=%d session=%s outcome=%s cause=%s\n", c.PID, c.CallID, c.Session, c.Outcome, cause)
		}
	}
	if !k.Available {
		fmt.Printf("soak kernel-udp available=false reason=%q\n", k.Reason)
		return
	}
	udp6 := "unavailable"
	if k.UDP6 {
		udp6 = fmt.Sprint(k.UDP6RcvbufErrors)
	}
	fmt.Printf("soak kernel-udp available=true scope=%q udp-rcvbuf-errors=%d udp6-rcvbuf-errors=%s\n", k.Scope, k.RcvbufErrors, udp6)
}
