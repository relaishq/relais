//go:build linux || darwin

package main

import (
	"context"
	"github.com/relais/internal/processidentity"
	"github.com/relais/internal/relaylease"
)

func acquireLease(ctx context.Context, standby bool, hostID string, cfg relaylease.Config) (commandGuard, error) {
	return acquireLeaseWithIdentity(ctx, standby, hostID, cfg, processidentity.CurrentWithHostID)
}

func validatePlatform(bool) error { return nil }
