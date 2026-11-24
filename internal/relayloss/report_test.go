package relayloss

import (
	"testing"
	"time"
)

func TestHostLossRequiresEveryCallWithinBudget(t *testing.T) {
	good := func() Report {
		r := Report{Completed: true}
		for i := range CallCount {
			r.Calls = append(r.Calls, Call{Number: i + 1, Gap: Budget, Recovered: true})
		}
		return r
	}
	if !good().Passed() {
		t.Fatal("budget boundary must pass")
	}
	for _, mutate := range []func(*Report){
		func(r *Report) { r.Completed = false },
		func(r *Report) { r.FailedPhase = "recovery" },
		func(r *Report) { r.Calls = r.Calls[:7] },
		func(r *Report) { r.Calls[7].Gap = Budget + time.Nanosecond },
		func(r *Report) { r.Calls[7].Gap = 0 },
		func(r *Report) { r.Calls[7].Recovered = false },
		func(r *Report) { r.Calls[7].Number = 1 },
	} {
		r := good()
		mutate(&r)
		if r.Passed() {
			t.Fatal("incomplete or over-budget drill passed", r)
		}
	}
}

func TestReportPhaseProjectionAcceptsOnlyFixedCodes(t *testing.T) {
	for _, phase := range []string{"setup", "baseline", "fault", "recovery", "restore", "observation", "completion", "unavailable", ""} {
		if PublicPhase(phase) != phase {
			t.Fatal("fixed diagnostic phase refused", phase)
		}
	}
	if PublicPhase("private-value") != "unavailable" {
		t.Fatal("free text reached diagnostics")
	}
}
