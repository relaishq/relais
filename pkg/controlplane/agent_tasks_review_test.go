package controlplane

import (
	"errors"
	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTaskReviewCapacityExportIsDefinitive(t *testing.T) {
	require.False(t, uncertainExport(agent.ErrCapacity))
	require.True(t, uncertainExport(errors.Join(agent.ErrCapacity, privateapi.ErrUncertain)))
}
