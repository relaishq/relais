package main

import (
	"context"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
)

func relayStandbyTrial(context.Context, *clusterprocess.Manager, string, string, []string, time.Duration, time.Duration, bool, bool) (result, error) {
	return result{}, processidentity.ErrUnsupported
}

func printRelayStandbyTable(string, []result) {}
