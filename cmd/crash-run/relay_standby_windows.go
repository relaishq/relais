package main

import (
	"context"
	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/processidentity"
	"time"
)

func relayStandbyTrial(context.Context, *clusterprocess.Manager, string, string, []string, time.Duration, time.Duration, bool, bool) (result, error) {
	return result{}, processidentity.ErrUnsupported
}

func printRelayStandbyTable(string, []result) {}
