package main

import (
	"fmt"
	"time"
)

func (e *Endpoint) statsLine() string {
	s := &e.Stats

	return fmt.Sprintf("%s: stun req/resp/bad=%d/%d/%d rtp in/out=%d/%d rtcp in/out=%d/%d srtpFail=%d srtcpFail=%d nackIn=%d rrIn=%d srIn=%d droppedNotReady=%d",
		e.name, s.StunReq.Load(), s.StunResp.Load(), s.StunBad.Load(), s.RTPIn.Load(), s.RTPOut.Load(),
		s.RTCPIn.Load(), s.RTCPOut.Load(), s.SRTPFail.Load(), s.SRTCPFail.Load(), s.NACKIn.Load(),
		s.RRIn.Load(), s.SRIn.Load(), s.DroppedNotReady.Load())
}

func rel(t, base time.Time) string { return fmt.Sprintf("%+.3fs", t.Sub(base).Seconds()) }

func report(c *Client, last *Endpoint, moves []moveRec, connectedAt, t0, lastMove, pauseStart, pauseEnd, endAt time.Time,
	closeResult string, buffered int64,
) {
	c.mu.Lock()
	echoes := append([]echoPkt{}, c.echoes...)
	states := append([]stateEvent{}, c.states...)
	consent := append([]consentSample{}, c.consent...)
	sent, srRecv, negNeeded := c.sent, c.srRecv, c.negNeeded
	c.mu.Unlock()

	pass := true
	fail := func(f string, a ...any) {
		pass = false
		fmt.Printf("  FAIL: "+f+"\n", a...)
	}

	fmt.Println()
	fmt.Println("==================== REPORT ====================")
	fmt.Printf("media start t0; moves at %s and %s; last move +%v STUN-only pause [%s, %s]; end %s\n",
		rel(moves[0].cutAt, t0), rel(moves[len(moves)-1].cutAt, t0), *flagPauseAt, rel(pauseStart, t0), rel(pauseEnd, t0), rel(endAt, t0))
	fmt.Printf("flags: margin=%d rtcp-margin=%d naive-roc=%v lag=%v close=%s\n", *flagMargin, *flagRTCPMargin, *flagNaiveROC, *flagLag, *flagClose)

	fmt.Println("\n-- client connection state events after connect (before teardown) --")
	badState := 0
	for _, s := range states {
		if s.at.Before(connectedAt) || s.at.After(endAt) {
			continue
		}
		fmt.Printf("  %s %s -> %s\n", rel(s.at, t0), s.kind, s.val)
		badState++
	}
	if badState > 0 {
		fail("%d client state changes after connect (want 0: no disconnect, no renegotiation, no ICE restart)", badState)
	} else {
		fmt.Println("  none (stayed connected; no signaling/negotiationneeded/ICE/DTLS changes)")
	}
	if negNeeded > 0 {
		fmt.Printf("  note: negotiationneeded fired %d times in total (incl. setup)\n", negNeeded)
	}

	fmt.Println("\n-- moves --")
	var gaps []time.Duration
	for i, m := range moves {
		fmt.Printf("  move %d %s: cut %s, snapshot taken %s before cut, cut->resumed %v, blob %dB (dtls %dB)\n",
			i+1, m.label, rel(m.cutAt, t0), m.cutAt.Sub(m.snapAt).Round(time.Microsecond),
			m.resumedAt.Sub(m.cutAt).Round(time.Microsecond), m.blobLen, m.dtlsLen)
		if i < 2 {
			fmt.Printf("    old endpoint stats at kill: %s\n", m.oldStats)
		}
		if i < 2 {
			fmt.Printf("    state (dtls bytes elided): %s\n", m.stateJSON)
		}
		// Largest inter-arrival among echoes arriving in [cut-50ms, cut+1s].
		var worst [2]*echoPkt
		var worstGap time.Duration
		var jump [2]*echoPkt
		for j := 1; j < len(echoes); j++ {
			a, b := &echoes[j-1], &echoes[j]
			if b.at.Before(m.cutAt.Add(-50*time.Millisecond)) || b.at.After(m.cutAt.Add(time.Second)) {
				continue
			}
			if g := b.at.Sub(a.at); g > worstGap {
				worstGap, worst = g, [2]*echoPkt{a, b}
			}
			if jump[0] == nil && b.seq-a.seq != 1 {
				jump = [2]*echoPkt{a, b}
			}
		}
		if worst[0] == nil {
			fail("move %d: no echo around the move", i+1)

			continue
		}
		gaps = append(gaps, worstGap)
		dc := worst[1].counter - worst[0].counter
		tsOK := worst[1].ts-worst[0].ts == dc*960
		fmt.Printf("    client echo gap: %v (normal 20ms); caller packets missing in that gap: %d; ts delta %d (expected %d, continuous=%v)\n",
			worstGap.Round(time.Millisecond), dc-1, worst[1].ts-worst[0].ts, dc*960, tsOK)
		if jump[0] != nil {
			fmt.Printf("    echo seq jump at move: %d -> %d (+%d), ts delta %d for caller counter delta %d\n",
				jump[0].seq, jump[1].seq, uint16(jump[1].seq-jump[0].seq), jump[1].ts-jump[0].ts, jump[1].counter-jump[0].counter)
		}
		if !tsOK {
			fail("move %d: timestamps not continuous", i+1)
		}
		if i == 0 {
			fmt.Printf("    DTLS blob: %s\n", m.dtlsDesc)
		}
	}
	if len(gaps) > 2 {
		var sum, mx time.Duration
		for _, g := range gaps {
			sum += g
			if g > mx {
				mx = g
			}
		}
		fmt.Printf("  %d moves: mean worst-gap %v, max %v\n", len(gaps), (sum / time.Duration(len(gaps))).Round(time.Millisecond), mx.Round(time.Millisecond))
	}

	fmt.Println("\n-- echo stream continuity (client side) --")
	tsBad, seqJumps, maxGap := 0, 0, time.Duration(0)
	for j := 1; j < len(echoes); j++ {
		a, b := echoes[j-1], echoes[j]
		dc := b.counter - a.counter
		if b.ts-a.ts != dc*960 {
			tsBad++
		}
		if uint16(b.seq-a.seq) != 1 {
			seqJumps++
			fmt.Printf("  seq discontinuity at %s: %d -> %d (caller counter %d -> %d)\n", rel(b.at, t0), a.seq, b.seq, a.counter, b.counter)
		}
		inPause := b.at.After(pauseStart) && a.at.Before(pauseEnd.Add(100*time.Millisecond))
		if g := b.at.Sub(a.at); g > maxGap && !inPause {
			maxGap = g
		}
	}
	fmt.Printf("  caller sent %d packets; echoes received %d; ts violations %d; seq discontinuities %d; max inter-arrival outside pause %v\n",
		sent, len(echoes), tsBad, seqJumps, maxGap.Round(time.Millisecond))
	fmt.Printf("  client received %d SRTCP sender reports from server\n", srRecv)
	if len(echoes) > 0 && echoes[len(echoes)-1].at.Before(lastMove.Add(*flagAfter-2*time.Second)) {
		fail("echo stopped before the end of the call")
	}
	if tsBad > 0 {
		fail("timestamp discontinuities")
	}

	fmt.Println("\n-- client SRTP/SRTCP decrypt / replay failures (srtp logger) --")
	sf := c.srtpFailures()
	for i, e := range sf {
		if i < 15 {
			fmt.Printf("  %s %s\n", rel(e.at, t0), e.msg)
		}
	}
	fmt.Printf("  total: %d\n", len(sf))
	if len(sf) > 0 {
		fail("client saw %d SRTP/SRTCP failures", len(sf))
	}
	fmt.Println("-- client ICE/DTLS/other warnings --")
	ws := c.warnings()
	for i, e := range ws {
		if i < 15 && e.at.Before(endAt) {
			fmt.Printf("  %s [%s/%s] %s\n", rel(e.at, t0), e.scope, e.level, e.msg)
		}
	}

	fmt.Println("\n-- consent (client's selected-pair STUN counters) --")
	var atLast, atEnd consentSample
	var longest time.Duration
	var lastInc time.Time
	pauseState := "n/a"
	for _, s := range consent {
		if s.at.Before(lastMove) {
			atLast = s
			lastInc = s.at

			continue
		}
		if s.responses > atLast.responses && lastInc.IsZero() {
			lastInc = s.at
		}
		if s.at.Before(endAt) {
			if s.responses > atEnd.responses {
				if g := s.at.Sub(lastInc); g > longest {
					longest = g
				}
				lastInc = s.at
			}
			atEnd = s
			if s.at.After(pauseStart) && s.at.Before(pauseEnd) && s.state != "connected" {
				pauseState = s.state
			}
		}
	}
	if pauseState == "n/a" {
		pauseState = "connected throughout"
	}
	fmt.Printf("  at last move: requests=%d responses=%d; at end (%v later): requests=%d responses=%d\n",
		atLast.requests, atLast.responses, atEnd.at.Sub(lastMove).Round(time.Second), atEnd.requests, atEnd.responses)
	fmt.Printf("  longest interval without a new STUN response after last move: %v (sampled every 500ms)\n", longest.Round(time.Millisecond))
	fmt.Printf("  client PeerConnection state during STUN-only pause (%v, > 5s disconnect timeout): %s\n", *flagPauseFor, pauseState)
	if atEnd.responses <= atLast.responses {
		fail("no STUN responses after last move")
	}
	if pauseState != "connected throughout" {
		fail("client left connected during STUN-only pause")
	}
	if atEnd.at.Sub(lastMove) < 30*time.Second {
		fail("observed less than 30s after last move")
	}

	fmt.Println("\n-- server endpoints --")
	fmt.Printf("  final %s\n", last.statsLine())
	fmt.Printf("  packets queued by demux while no endpoint owned the session (delivered to new owner): %d\n", buffered)
	if last.Stats.SRTPFail.Load() > 0 || last.Stats.SRTCPFail.Load() > 0 {
		fail("server decrypt failures on final endpoint")
	}
	for _, m := range moves {
		if m.oldFails > 0 {
			fail("server decrypt failures on an endpoint (%s)", m.label)
		}
	}

	fmt.Println("\n-- DTLS end-of-call check --")
	fmt.Printf("  %s\n", closeResult)
	if len(closeResult) >= 4 && closeResult[:4] == "FAIL" {
		pass = false
	}

	fmt.Println()
	if pass {
		fmt.Println("RESULT: PASS")
	} else {
		fmt.Println("RESULT: FAIL")
	}
}
