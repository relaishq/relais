package main

import (
	"context"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
	"time"
)

func runRelayLossTrials(context.Context, *clusterprocess.Manager, string, string, string, string, int, time.Duration) error {
	return processidentity.ErrUnsupported
}
