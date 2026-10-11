package main

import (
	"context"
	"errors"
	"github.com/relais/internal/processidentity"
	"github.com/relais/internal/relaylease"
	"github.com/relais/pkg/sessionstore"
	"log"
)

type commandGuard interface {
	Context() context.Context
	Allowed() bool
	Lease() sessionstore.RelayLease
	Timing() relaylease.Timing
	Close()
	Release(context.Context) error
	Err() error
	OnLost(func())
}

func acquireLeaseWithIdentity(ctx context.Context, standby bool, hostID string, cfg relaylease.Config, current func(string) (processidentity.Identity, error)) (commandGuard, error) {
	id, err := current(hostID)
	if err != nil {
		return nil, err
	}
	if id.Host == processidentity.UnknownHost {
		if standby {
			return nil, errors.New("standby requires a host identity; set -host-id or RELAIS_HOST_ID")
		}
		log.Printf("relay host identity unknown; single-relay mode only; set -host-id or RELAIS_HOST_ID before using a standby")
	}
	cfg.Process.PID, cfg.Process.Start = id.PID, id.Start
	cfg.Process.Host, cfg.Process.Namespace, cfg.Process.Executable = id.Host, id.Namespace, id.Executable
	cfg.AllowUnknownHostRestart = !standby
	return relaylease.Acquire(ctx, cfg)
}
