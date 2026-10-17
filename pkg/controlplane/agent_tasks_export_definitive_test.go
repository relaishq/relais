package controlplane

import (
	"errors"
	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTaskCapacityExportIsDefinitive(t *testing.T) {
	require.False(t, uncertainExport(agent.ErrCapacity))
	require.True(t, uncertainExport(errors.Join(agent.ErrCapacity, privateapi.ErrUncertain)))
}

func TestTaskIncompatibleExportIsDefinitive(t *testing.T) {
	require.False(t, uncertainExport(mediaworker.ErrExportIncompatible))
	require.True(t, uncertainExport(errors.Join(mediaworker.ErrExportIncompatible, privateapi.ErrUncertain)))
}
