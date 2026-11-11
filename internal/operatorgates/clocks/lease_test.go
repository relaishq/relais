package clocks

import "testing"

func TestExpiredLeaseAge(t *testing.T) {
	o := LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: "1792886524000000000", ReferenceUnixNS: "1792886519800000000", ReferenceAgeNS: 4200000000, PollSeconds: "1", ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 1e9, HostElapsedNS: 1e9}
	age, budget, ok := ExpiredLeaseAge(o)
	if !ok || age != 5200000000 || budget != 5000000000 {
		t.Fatal(age, budget, ok)
	}
	o.ReferenceAgeNS, o.ReferenceUnixNS = 3000000000, "1792886521000000000"
	if _, _, ok = ExpiredLeaseAge(o); ok {
		t.Fatal("fresh lease reported expired")
	}
}
