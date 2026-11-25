package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayLossCallerAdmissionIsExplicit(t *testing.T) {
	for _, args := range [][]string{
		{"-relay-loss-caller"},
		{"-relay-loss-caller", "-external-caller", "-callers=1", "-gomaxprocs=2"},
		{"-relay-loss-caller", "-external-caller", "-callers=4", "-gomaxprocs=2", "-distributed"},
		{"-relay-loss-caller", "-external-caller", "-callers=4", "-gomaxprocs=2", "-video=false"},
	} {
		err := runArgs(args)
		require.ErrorContains(t, err, "relay-loss caller requires")
	}
	err := runArgs([]string{"-relay-loss-caller", "-external-caller", "-callers=4", "-gomaxprocs=2", "-relay=192.0.2.1:3478", "-output=" + filepath.Join(t.TempDir(), "caller.jsonl")})
	require.ErrorContains(t, err, "non-loopback relay requires -caller-bind", "R admission reaches the existing endpoint checks")
}
