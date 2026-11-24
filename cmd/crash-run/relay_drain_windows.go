package main

import (
	"context"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
)

func runRelayDrainTrials(context.Context, *clusterprocess.Manager, string, string, string, string, int, time.Duration) error {
	return processidentity.ErrUnsupported
}
