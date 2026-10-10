package relay

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlowRouteStickiness(t *testing.T) {
	caller := netip.MustParseAddrPort("192.0.2.1:5000")
	a := netip.MustParseAddrPort("127.0.0.1:40001")
	b := netip.MustParseAddrPort("127.0.0.1:40002")
	now := time.Now()
	newTable := func(t *testing.T) *flowTable {
		t.Helper()
		table := newFlowTable(flowLimits{stickinessWindow: time.Second,
			idleTimeout: 10 * time.Second, pendingTimeout: 3 * time.Second, maxFlows: 8, maxPending: 4})
		request := bindingRequest(t, sessionA)
		_, tx, _ := parseBindingRequest(request)
		require.True(t, table.admit(caller, a, sessionA, tx, now))
		allowed, promoted := table.answer(caller, a, bindingSuccess(t, request), now)
		require.True(t, allowed)
		require.Equal(t, sessionA, promoted)
		return table
	}
	check := func(t *testing.T, table *flowTable, session string, at time.Time, answer bool) []byte {
		t.Helper()
		request := bindingRequest(t, session)
		_, tx, _ := parseBindingRequest(request)
		worker, ok := table.routeSTUN(caller, session, tx, at)
		require.True(t, ok)
		if answer {
			allowed, _ := table.answer(caller, worker, bindingSuccess(t, request), at)
			require.True(t, allowed)
		}
		return request
	}
	claimB := func(t *testing.T, table *flowTable, at time.Time, accepted bool) {
		t.Helper()
		request := bindingRequest(t, sessionB)
		_, tx, _ := parseBindingRequest(request)
		require.Equal(t, accepted, table.admit(caller, b, sessionB, tx, at))
		allowed, promoted := table.answer(caller, b, bindingSuccess(t, request), at)
		if accepted || table.callers[caller].route.worker != b {
			require.Equal(t, accepted, allowed)
		}
		if accepted {
			require.Equal(t, sessionB, promoted)
		} else {
			require.Empty(t, promoted)
			require.Equal(t, sessionA, table.callers[caller].route.session)
		}
	}

	t.Run("authenticated incumbent refresh blocks own-credential spoof", func(t *testing.T) {
		table := newTable(t)
		check(t, table, sessionA, now.Add(900*time.Millisecond), true)
		claimB(t, table, now.Add(1100*time.Millisecond), false)
		worker, ok := table.route(caller, now.Add(1200*time.Millisecond))
		require.True(t, ok)
		require.Equal(t, a, worker)
	})
	t.Run("media and unanswered requests do not keep the address", func(t *testing.T) {
		table := newTable(t)
		_, ok := table.route(caller, now.Add(900*time.Millisecond))
		require.True(t, ok)
		check(t, table, sessionA, now.Add(950*time.Millisecond), false)
		claimB(t, table, now.Add(time.Second), true)
	})
	t.Run("unmatched and replayed successes do not refresh", func(t *testing.T) {
		table := newTable(t)
		request := check(t, table, sessionA, now.Add(100*time.Millisecond), true)
		allowed, _ := table.answer(caller, a, bindingSuccess(t, request), now.Add(time.Second))
		require.True(t, allowed)
		allowed, _ = table.answer(caller, a, bindingSuccess(t, bindingRequest(t, sessionA)), now.Add(time.Second))
		require.True(t, allowed)
		claimB(t, table, now.Add(1100*time.Millisecond), true)
	})
	t.Run("delayed success uses the request time", func(t *testing.T) {
		table := newTable(t)
		request := check(t, table, sessionA, now.Add(100*time.Millisecond), false)
		allowed, _ := table.answer(caller, a, bindingSuccess(t, request), now.Add(time.Second))
		require.True(t, allowed)
		claimB(t, table, now.Add(1100*time.Millisecond), true)
	})
	t.Run("incumbent returning before candidate success stops promotion", func(t *testing.T) {
		for _, worker := range []netip.AddrPort{b, a} {
			table := newTable(t)
			request := bindingRequest(t, sessionB)
			_, tx, _ := parseBindingRequest(request)
			require.True(t, table.admit(caller, worker, sessionB, tx, now.Add(time.Second)))
			check(t, table, sessionA, now.Add(1100*time.Millisecond), true)
			allowed, promoted := table.answer(caller, worker, bindingSuccess(t, request), now.Add(1200*time.Millisecond))
			require.False(t, allowed)
			require.Empty(t, promoted)
			require.Equal(t, sessionA, table.callers[caller].route.session)
			require.Zero(t, table.counts().pending)
		}
	})
	t.Run("same session can follow a new worker", func(t *testing.T) {
		table := newTable(t)
		request := bindingRequest(t, sessionA)
		_, tx, _ := parseBindingRequest(request)
		require.True(t, table.admit(caller, b, sessionA, tx, now))
		allowed, promoted := table.answer(caller, b, bindingSuccess(t, request), now)
		require.True(t, allowed)
		require.Equal(t, sessionA, promoted)
		require.Equal(t, b, table.callers[caller].route.worker)
	})
	t.Run("trusted move retains stickiness and fences old checks", func(t *testing.T) {
		table := newTable(t)
		request := check(t, table, sessionA, now.Add(100*time.Millisecond), false)
		table.moveSession(sessionA, a, b)
		allowed, _ := table.answer(caller, a, bindingSuccess(t, request), now.Add(900*time.Millisecond))
		require.False(t, allowed)
		require.Equal(t, now, table.callers[caller].lastAuthenticated)
		claimB(t, table, now.Add(900*time.Millisecond), false)
	})
	t.Run("forget releases the address immediately", func(t *testing.T) {
		table := newTable(t)
		table.forgetSession(sessionA)
		claimB(t, table, now, true)
		require.NotContains(t, table.sessions, sessionA)
	})
	t.Run("idle cleanup cannot remove fresh stickiness", func(t *testing.T) {
		table := newTable(t)
		table.idleTimeout = 100 * time.Millisecond
		table.sweep(now.Add(500 * time.Millisecond))
		claimB(t, table, now.Add(600*time.Millisecond), false)
		table.sweep(now.Add(time.Second))
		require.Empty(t, table.callers)
		require.Empty(t, table.sessions)
	})
}

func TestRouteConsentTransactionsAreBounded(t *testing.T) {
	var checks bindingChecks
	now := time.Now()
	first := bindingRequest(t, sessionA)
	_, firstID, _ := parseBindingRequest(first)
	checks.record(firstID, now)
	checks.record(firstID, now.Add(time.Second))
	at, ok := checks.answer(firstID)
	require.True(t, ok)
	require.Equal(t, now, at, "retransmission cannot extend request time")
	_, ok = checks.answer(firstID)
	require.False(t, ok, "success consumed exactly once")
	checks.record(firstID, now)
	for range maxOutstandingChecks {
		_, id, _ := parseBindingRequest(bindingRequest(t, sessionA))
		checks.record(id, now)
	}
	_, ok = checks.answer(firstID)
	require.False(t, ok, "oldest transaction evicted")
}
