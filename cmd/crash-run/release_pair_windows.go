package main

import (
	"context"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
)

// The release pair needs the same-host relay standby, which Windows lacks.
func runReleasePair(context.Context, *clusterprocess.Manager, string, releasePairOptions) error {
	return processidentity.ErrUnsupported
}
