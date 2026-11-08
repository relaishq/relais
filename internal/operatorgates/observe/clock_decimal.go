package observe

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
)

var clockDecimal = regexp.MustCompile(`^[0-9]\.[0-9]{3}e([+-][0-9]{2,3})$`)

// ClockMaximumNS preserves column 14's half-unit printing uncertainty.
func ClockMaximumNS(raw string) (*big.Rat, error) {
	m := clockDecimal.FindStringSubmatch(raw)
	if m == nil {
		return nil, fmt.Errorf("clock-select-column14")
	}
	exponent, _ := strconv.Atoi(m[1])
	if exponent < -60 || exponent > 8 {
		return nil, fmt.Errorf("clock-select-column14")
	}
	n, _ := new(big.Rat).SetString(raw)
	pad, _ := new(big.Rat).SetString("5e" + strconv.Itoa(exponent-4))
	n.Add(n, pad)
	return n.Mul(n, big.NewRat(1e9, 1)), nil
}
