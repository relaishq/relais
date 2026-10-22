package controlplane

import (
	"errors"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
)

func TestTaskCapacityExportIsDefinitive(t *testing.T) {
	requireExportOutcome(t, agent.ErrCapacity, false)
	requireExportOutcome(t, errors.Join(agent.ErrCapacity, privateapi.ErrUncertain), true)
}

func TestTaskIncompatibleExportIsDefinitive(t *testing.T) {
	requireExportOutcome(t, mediaworker.ErrExportIncompatible, false)
	requireExportOutcome(t, errors.Join(mediaworker.ErrExportIncompatible, privateapi.ErrUncertain), true)
}
