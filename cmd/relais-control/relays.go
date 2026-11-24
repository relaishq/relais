package main

import (
	"context"
	"errors"
	"strings"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
)

type relayEndpoint struct {
	id     string
	remote *controlplane.RemoteRelay
}

func parseRelays(entries []string) ([]relayEndpoint, error) {
	if len(entries) == 1 && entries[0] != "" && !strings.Contains(entries[0], "=") {
		return []relayEndpoint{{remote: &controlplane.RemoteRelay{URL: entries[0]}}}, nil
	}
	// Relay names and URLs have the same syntax and uniqueness rules as fixed
	// inbound workers. A name is the slot's shared relay lease key.
	fixed, err := parseWorkers(entries)
	if err != nil || len(fixed) == 0 {
		return nil, errors.New("relay must be name=http://address, repeated with unique names and URLs")
	}
	var result []relayEndpoint
	for _, entry := range fixed {
		if entry.endpoint == "dial-out" {
			return nil, errors.New("relay must be name=http://address")
		}
		result = append(result, relayEndpoint{id: entry.name, remote: &controlplane.RemoteRelay{URL: entry.endpoint}})
	}
	return result, nil
}

func configureRelaySet(ctx context.Context, plane *controlplane.Plane, relays []relayEndpoint) error {
	// Preserve legacy placement, capability negotiation and loss detection for
	// one relay, including the named form. Only several relays enable a set.
	if len(relays) < 2 {
		return nil
	}
	bindings := make([]controlplane.RelayBinding, 0, len(relays))
	for _, entry := range relays {
		bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: entry.id}, Relay: entry.remote})
	}
	return plane.ConfigureRelaySet(ctx, bindings)
}
