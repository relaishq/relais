package main

import (
	"context"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/internal/processidentity"
)

func runRelayLossTrials(context.Context, *clusterprocess.Manager, string, string, string, string, int, time.Duration, bool, *nettopology.Topology, string, relayLossOptions) error {
	return processidentity.ErrUnsupported
}
