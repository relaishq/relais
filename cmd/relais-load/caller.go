package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/relais/internal/nettopology"
	"github.com/relais/pkg/callharness"
)

func externalTopology(signaling string, relay netip.AddrPort) callharness.ExternalTopology {
	return callharness.ExternalTopology{SignalingURL: signaling, RelayAddr: relay}
}

func validateExternalCaller(fs *flag.FlagSet, c config) error {
	allowed := map[string]bool{
		"external-caller": true, "signaling": true, "relay": true,
		"caller-namespace": true, "caller-bind": true, "signaling-cert": true,
		"bearer-token-file": true, "parent-pid": true, "callers": true,
		"gomaxprocs": true, "duration": true, "sample-every": true,
		"video": true, "output": true, "seed": true, "caller-process": true,
		"distributed": true, "run-id": true, "frontend-control-bound": true, "frontend-worker-bound": true, "clock-drift-ppm": true, "event-retention": true,
	}
	var unsupported error
	fs.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] && unsupported == nil {
			unsupported = fmt.Errorf("-%s is not supported with -external-caller", f.Name)
		}
	})
	if unsupported != nil {
		return unsupported
	}
	if c.Callers < 1 || c.GOMAXPROCS < 1 || c.Duration < time.Second || c.SampleEvery < 100*time.Millisecond || c.SampleEvery > c.Duration || c.Output == "" {
		return errors.New("external caller needs callers and gomaxprocs >= 1, duration >= 1s, sample interval in [100ms,duration] and an output file")
	}
	return nil
}

func childTopology(signaling string, relay netip.AddrPort, namespace, bind string) (callharness.ExternalTopology, error) {
	external := externalTopology(signaling, relay)
	if namespace == "" && bind == "" {
		return external, nil
	}
	addr, err := netip.ParseAddrPort(bind)
	if err != nil || !addr.Addr().Is4() || addr.Addr().IsUnspecified() {
		return external, errors.New("caller requires a specific IPv4 UDP bind address")
	}
	external.CallerSocket = func() (net.PacketConn, error) {
		if namespace == "" {
			return net.ListenPacket("udp4", addr.String())
		}
		return nettopology.CallerSocketInNamespace(namespace, addr)
	}
	return external, nil
}
