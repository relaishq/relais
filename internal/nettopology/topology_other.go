//go:build !linux

package nettopology

import (
	"context"
	"errors"
	"net"
)

var errLinux = errors.New("netns topology requires Linux; run it inside a Linux VM or container (no Mac network changes)")

func Open(context.Context, func(string, ...any)) (*Topology, error) { return nil, errLinux }
func withNamespace(string, func() error) error                      { return errLinux }
func (t *Topology) CallerSocket() (net.PacketConn, error)           { return nil, errLinux }
func (t *Topology) removeNamespace(string) error                    { return errLinux }

func OpenManaged(context.Context, func(string, ...any), func(func()) error) (*Topology, error) {
	return nil, errLinux
}
