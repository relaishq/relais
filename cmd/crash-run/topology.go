package main

import (
	"errors"
	"net"
	"net/netip"
	"strings"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/nettopology"
)

func validateTopologyOptions(topology, profile, redis string) error {
	if topology != "loopback" && topology != "netns" {
		return errors.New("topology must be loopback or netns")
	}
	if profile != "lan" {
		return errors.New("only the unshaped lan profile is supported")
	}
	if topology == "netns" && redis != "" {
		return errors.New("netns topology starts its own namespaced store; omit -redis and RELAIS_REDIS_ADDR")
	}
	return nil
}
func roleBind(t *nettopology.Topology, role string, public bool) string {
	if t == nil {
		return "127.0.0.1:0"
	}
	ip := t.Plan.Roles[role].Private
	if public {
		ip = t.Plan.Roles[role].Public
	}
	return netip.AddrPortFrom(ip, 0).String()
}
func controlAddress(t *nettopology.Topology) (string, error) {
	if t == nil {
		return clusterprocess.FreeTCP()
	}
	return net.JoinHostPort(t.Plan.Roles["control"].Private.String(), "40001"), nil
}
func redisAddress(t *nettopology.Topology) (string, error) {
	if t == nil {
		return clusterprocess.FreeTCP()
	}
	return net.JoinHostPort(t.Plan.Roles["store"].Private.String(), "16379"), nil
}
func redisHost(t *nettopology.Topology) string {
	if t == nil {
		return "127.0.0.1"
	}
	return t.Plan.Roles["store"].Private.String()
}
func startRole(manager *clusterprocess.Manager, t *nettopology.Topology, dir, name string, env []string, args ...string) (*clusterprocess.Child, error) {
	if t == nil {
		return manager.Start(dir, name, env, args...)
	}
	role := name
	if role == "redis" {
		role = "store"
	}
	return manager.StartInNamespace(dir, name, t.Plan.Roles[role].Namespace, env, args...)
}
func envValue(env []string, key string) string {
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if name == key {
			return value
		}
	}
	return ""
}
