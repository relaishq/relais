//go:build !linux && !darwin

package main

import (
	"context"
	"github.com/relais/internal/processidentity"
	"github.com/relais/internal/relaylease"
	"github.com/relais/pkg/sessionstore"
)

// Unsupported platforms retain plain relay operation without process fencing.
func acquireLease(ctx context.Context, standby bool, _ relaylease.Config) (commandGuard, error) {
	if standby {
		return nil, processidentity.ErrUnsupported
	}
	return unfencedGuard{ctx}, nil
}

type unfencedGuard struct{ ctx context.Context }

func (g unfencedGuard) Context() context.Context     { return g.ctx }
func (g unfencedGuard) Allowed() bool                { return g.ctx.Err() == nil }
func (unfencedGuard) Lease() sessionstore.RelayLease { return sessionstore.RelayLease{} }
func (unfencedGuard) Timing() relaylease.Timing      { return relaylease.Timing{} }
func (unfencedGuard) Close()                         {}
func (unfencedGuard) Release(context.Context) error  { return nil }
func (unfencedGuard) Err() error                     { return nil }
func (unfencedGuard) OnLost(func())                  {}

func validatePlatform(standby bool) error {
	if standby {
		return processidentity.ErrUnsupported
	}
	return nil
}
