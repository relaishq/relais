package main

import (
	"context"
	"github.com/relais/internal/relaylease"
	"github.com/relais/pkg/sessionstore"
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
