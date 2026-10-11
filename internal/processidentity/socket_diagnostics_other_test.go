//go:build !linux

package processidentity

import "testing"

func logSocketExitState(*testing.T, int, string) {}
