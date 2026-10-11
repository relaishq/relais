//go:build !linux && !darwin

package main

import (
	"context"
	"testing"

	"github.com/relais/internal/processidentity"
	"github.com/relais/internal/relaylease"
	"github.com/stretchr/testify/require"
)

func TestUnsupportedPlatformKeepsPlainRelayAndRefusesStandby(t *testing.T) {
	require.NoError(t, validatePlatform(false))
	require.ErrorIs(t, validatePlatform(true), processidentity.ErrUnsupported)
	g, err := acquireLease(context.Background(), false, relaylease.Config{})
	require.NoError(t, err)
	require.True(t, g.Allowed())
	g.Close()
	_, err = acquireLease(context.Background(), true, relaylease.Config{})
	require.ErrorIs(t, err, processidentity.ErrUnsupported)
}
