//go:build linux || darwin

package main

import (
	"context"
	"github.com/relais/internal/processidentity"
	"github.com/relais/internal/relaylease"
)

func acquireLease(ctx context.Context, _ bool, cfg relaylease.Config) (commandGuard, error) {
	id, err := processidentity.Current()
	if err != nil {
		return nil, err
	}
	cfg.Process.PID, cfg.Process.Start = id.PID, id.Start
	cfg.Process.Host, cfg.Process.Namespace, cfg.Process.Executable = id.Host, id.Namespace, id.Executable
	return relaylease.Acquire(ctx, cfg)
}

func validatePlatform(bool) error { return nil }
