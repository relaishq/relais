package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/nettopology"
)

// NetworkCommands opens only the two caller TCP services and relay UDP media.
// Routing occurs inside the owned relay, never through the host management veth.
func NetworkCommands(p nettopology.Plan, c Config) []nettopology.Command {
	relay, caller := p.Roles["relay"], p.Roles["caller"]
	commands := []nettopology.Command{
		{"ip", "netns", "exec", relay.Namespace, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
		{"ip", "-n", caller.Namespace, "route", "add", p.Datacentre.String(), "via", relay.Public.String()},
	}
	for _, role := range []string{"store", "control", "worker-0", "worker-1"} {
		commands = append(commands, nettopology.Command{"ip", "-n", p.Roles[role].Namespace, "route", "add", p.CallerNet.String(), "via", relay.Private.String()})
	}
	for _, raw := range []string{c.RoomURL, c.SignalURL} {
		raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
		endpoint := netip.MustParseAddrPort(raw)
		commands = append(commands, nettopology.Command{"ip", "netns", "exec", caller.Namespace, "iptables", "-w", "2", "-A", "OUTPUT", "-p", "tcp", "-d", endpoint.Addr().String(), "--dport", fmt.Sprint(endpoint.Port()), "-j", "ACCEPT"})
	}
	media := netip.MustParseAddrPort(c.Media)
	commands = append(commands,
		nettopology.Command{"ip", "netns", "exec", caller.Namespace, "iptables", "-w", "2", "-A", "OUTPUT", "-p", "udp", "-d", media.Addr().String(), "--dport", fmt.Sprint(media.Port()), "-j", "ACCEPT"},
		nettopology.Command{"ip", "netns", "exec", caller.Namespace, "iptables", "-w", "2", "-A", "INPUT", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	)
	return commands
}

func networkCommand(ctx context.Context, c nettopology.Command) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(bounded, c[0], c[1:]...).CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("%v: %w: %s", c, err, b)
	}
	return strings.TrimSpace(string(b)), nil
}

func ConfigureNetwork(ctx context.Context, n *nettopology.Topology, c Config, profile, out string) error {
	var settings []nettopology.LinkSetting
	var err error
	var commands []nettopology.Command
	if profile == "poor-wifi" {
		settings, err = n.ApplyProfile(ctx, "lan")
		if err != nil {
			return err
		}
		for i := range settings {
			s := &settings[i]
			if s.Role != "caller" {
				continue
			}
			s.Delay, s.Jitter, s.Loss, s.Reorder = 15*time.Millisecond, 7500*time.Microsecond, 2, 1
			c := nettopology.Command{"ip", "netns", "exec", s.Namespace, "tc", "qdisc", "replace", "dev", s.Interface, "root", "netem", "limit", "10000", "delay", "15000us", "7500us", "loss", "2%", "25%", "reorder", "1%", "25%"}
			commands = append(commands, c)
			if _, err = networkCommand(ctx, c); err != nil {
				return err
			}
			s.Qdisc, err = networkCommand(ctx, nettopology.Command{"ip", "netns", "exec", s.Namespace, "tc", "qdisc", "show", "dev", s.Interface})
			if err != nil {
				return err
			}
			if err = s.VerifyQdisc(); err != nil {
				return err
			}
		}
	} else {
		settings, err = n.ApplyProfile(ctx, profile)
		if err != nil {
			return err
		}
	}
	commands = append(commands, NetworkCommands(n.Plan, c)...)
	// Applying Wi-Fi qdiscs above already ran those commands; repeat is harmless
	// but avoid restarting netem random state at this point.
	for _, cmd := range NetworkCommands(n.Plan, c) {
		if _, err = networkCommand(ctx, cmd); err != nil {
			return err
		}
	}
	routes, err := n.ObservePrivateRoutes(ctx)
	if err != nil {
		return err
	}
	observed := map[string]string{}
	for _, raw := range []string{strings.TrimPrefix(c.RoomURL, "http://"), c.Media} {
		endpoint := netip.MustParseAddrPort(raw)
		route, e := networkCommand(ctx, nettopology.Command{"ip", "netns", "exec", n.Plan.Roles["caller"].Namespace, "ip", "route", "get", endpoint.Addr().String()})
		if e != nil {
			return e
		}
		observed[raw] = route
		if endpoint.Addr() != n.Plan.Roles["relay"].Public && !strings.Contains(route, "via "+n.Plan.Roles["relay"].Public.String()) {
			return fmt.Errorf("caller room path bypasses relay: %s", route)
		}
	}
	evidence := map[string]any{"caller_routes": observed, "profile": profile, "plan": n.Plan, "settings": settings, "commands": commands, "private_routes": routes, "route_tuning": false}
	b, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "network-settings.json"), b, 0600)
}
