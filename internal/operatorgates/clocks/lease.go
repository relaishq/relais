package clocks

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// LeaseObservation is optional recorded evidence, never clock admission in 5a.
// Growth is the total root-dispersion growth, already including chrony's
// frequency uncertainty; residual/skew must not be added a second time.
type LeaseObservation struct {
	Available              bool   `json:"available"`
	Reason                 string `json:"reason"`
	ErrorBoundNS           int64  `json:"errorBoundNs"`
	MeasuredUnixNS         string `json:"measuredUnixNs"`
	ReferenceUnixNS        string `json:"referenceUnixNs"`
	ReferenceAgeNS         int64  `json:"referenceAgeNs"`
	PollSeconds            string `json:"pollSeconds"`
	ResidualPPM            string `json:"residualPpm"`
	SkewPPM                string `json:"skewPpm"`
	DispersionStartSeconds string `json:"dispersionStartSeconds"`
	DispersionEndSeconds   string `json:"dispersionEndSeconds"`
	IntervalNS             int64  `json:"intervalNs"`
	HostElapsedNS          int64  `json:"hostElapsedNs"`
}

type RecordedLease = observe.RecordedLease

var rationalPattern = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$|^[0-9]+/[1-9][0-9]*$`)

func exact(s string) (*big.Rat, error) {
	if len(s) > 80 || !rationalPattern.MatchString(s) {
		return nil, fmt.Errorf("invalid clock rational")
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(s[i+1:])
		if err != nil || exponent < -70 || exponent > 70 {
			return nil, fmt.Errorf("invalid clock rational")
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Num().BitLen() > 256 || r.Denom().BitLen() > 256 {
		return nil, fmt.Errorf("invalid clock rational")
	}
	return r, nil
}

func ceilNS(r *big.Rat) (int64, error) {
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	if r.Sign() < 0 || !n.IsInt64() {
		return 0, fmt.Errorf("clock bound overflow")
	}
	return n.Int64(), nil
}

// RecordLease uses host-only monotonic elapsed time in 5a. It does not measure
// gate-to-host transport. In 5b the gate must supply its complete monotonic age
// from request-send, including transport. Existing fault paths ignore this data.
func RecordLease(o LeaseObservation, ageNS int64) (RecordedLease, error) {
	var out RecordedLease
	if !o.Available || o.Reason != "" || o.ErrorBoundNS < 0 || o.ReferenceAgeNS < 0 || o.IntervalNS <= 0 || o.HostElapsedNS < o.IntervalNS || ageNS < o.HostElapsedNS || !nsPattern.MatchString(o.MeasuredUnixNS) || !nsPattern.MatchString(o.ReferenceUnixNS) {
		return out, fmt.Errorf("clock-lease-missing")
	}
	measured, measuredErr := strconv.ParseInt(o.MeasuredUnixNS, 10, 64)
	reference, referenceErr := strconv.ParseInt(o.ReferenceUnixNS, 10, 64)
	if measuredErr != nil || referenceErr != nil || reference > measured || measured-reference != o.ReferenceAgeNS {
		return out, fmt.Errorf("clock-lease-missing")
	}
	a, e1 := exact(o.DispersionStartSeconds)
	b, e2 := exact(o.DispersionEndSeconds)
	poll, e3 := exact(o.PollSeconds)
	_, e4 := exact(o.ResidualPPM)
	skew, e5 := exact(o.SkewPPM)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || a.Sign() < 0 || b.Cmp(a) < 0 || poll.Sign() <= 0 || skew.Sign() < 0 {
		return out, fmt.Errorf("clock-lease-missing")
	}
	// chronyc prints dispersion to nine decimal places. Both rounding errors
	// together add at most one nanosecond to the measured dispersion change.
	growth := new(big.Rat).Sub(b, a)
	growth.Add(growth, big.NewRat(1, 1e9))
	growth.Mul(growth, big.NewRat(1e15, o.IntervalNS))
	out.GrowthPPM = growth.RatString()
	aged := new(big.Rat).Mul(growth, big.NewRat(ageNS, 1e6))
	aged.Add(aged, big.NewRat(o.ErrorBoundNS, 1))
	var err error
	out.BoundNS, err = ceilNS(aged)
	if err != nil {
		return out, err
	}
	remaining := new(big.Rat).Mul(poll, big.NewRat(2e9, 1))
	remaining.Sub(remaining, big.NewRat(o.ReferenceAgeNS, 1))
	remaining.Sub(remaining, big.NewRat(ageNS, 1))
	// Round the remaining lifetime inward, so an imprecise poll never extends it.
	n := new(big.Int).Quo(remaining.Num(), remaining.Denom())
	if !n.IsInt64() {
		return out, fmt.Errorf("clock bound overflow")
	}
	out.RemainingNS = max(0, n.Int64())
	out.Expired = remaining.Sign() <= 0
	return out, nil
}
