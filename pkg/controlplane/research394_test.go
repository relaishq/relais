package controlplane

// RESEARCH394: temporary timing report for issue #394. Not for merge.

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func researchReport(t *testing.T, s *lossWorkStore, total time.Duration) {
	s.evMu.Lock()
	events := slices.Clone(s.events)
	s.evMu.Unlock()
	slices.SortFunc(events, func(a, b lossEvent) int { return int(a.start - b.start) })
	var first *lossEvent
	dispatches := map[string]bool{}
	for i := range events {
		if events[i].method == "CASRelay" && events[i].pending {
			dispatches[events[i].id] = true
			if first == nil || events[i].start+events[i].dur < first.start+first.dur {
				first = &events[i]
			}
		}
	}
	firstAt := map[string]time.Duration{}
	for _, e := range events {
		if e.method == "CASRelay" && e.pending {
			if v, ok := firstAt[e.id]; !ok || e.start < v {
				firstAt[e.id] = e.start
			}
		}
	}
	var ats []time.Duration
	for _, v := range firstAt {
		ats = append(ats, v)
	}
	slices.Sort(ats)
	var b strings.Builder
	if len(ats) > 0 {
		fmt.Fprintf(&b, "R394 dispatch_times n=%d first=%.1fms p50=%.1fms p90=%.1fms last=%.1fms\n", len(ats), ms(ats[0]), ms(ats[len(ats)/2]), ms(ats[len(ats)*9/10]), ms(ats[len(ats)-1]))
	}
	fmt.Fprintf(&b, "R394 total_observed=%s events=%d calls_dispatched=%d\n", total, len(events), len(dispatches))
	if first == nil {
		t.Log(b.String())
		return
	}
	cut := first.start
	// Processing order: first RelayOwner per call.
	var order []string
	seen := map[string]bool{}
	for _, e := range events {
		if e.method == "RelayOwner" && !seen[e.id] {
			seen[e.id] = true
			order = append(order, e.id)
		}
	}
	fmt.Fprintf(&b, "R394 first_dispatch id=%s at=%s rank_in_processing_order=%d/%d\n", first.id, cut, slices.Index(order, first.id)+1, len(order))
	for _, e := range events {
		if e.id == first.id && e.start <= cut {
			fmt.Fprintf(&b, "R394   chain %-22s start=%8.2fms dur=%7.2fms\n", e.method, ms(e.start), ms(e.dur))
		}
	}
	type agg struct {
		n   int
		ds  []time.Duration
		tot time.Duration
	}
	per := map[string]*agg{}
	ids := map[string]bool{}
	for _, e := range events {
		if e.start > cut {
			continue
		}
		ids[e.id] = true
		a := per[e.method]
		if a == nil {
			a = &agg{}
			per[e.method] = a
		}
		a.n++
		a.ds = append(a.ds, e.dur)
		a.tot += e.dur
	}
	fmt.Fprintf(&b, "R394 before_first_dispatch distinct_calls=%d\n", len(ids))
	var names []string
	for k := range per {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		a := per[k]
		slices.Sort(a.ds)
		fmt.Fprintf(&b, "R394   %-22s n=%4d mean=%6.2fms p50=%6.2fms p90=%6.2fms max=%6.2fms\n", k, a.n, ms(a.tot/time.Duration(a.n)), ms(a.ds[len(a.ds)/2]), ms(a.ds[len(a.ds)*9/10]), ms(a.ds[len(a.ds)-1]))
	}
	t.Log(b.String())
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
