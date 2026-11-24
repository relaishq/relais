package nettopology

import (
	"context"
	"errors"
)

func (p *Plan) addSpareRelay() {
	p.Roles["relay-spare"] = Role{Namespace: p.ID + "-relay-spare", Private: address(p.Datacentre, 5), Public: address(p.CallerNet, 4)}
	p.addLink("relay-spare", p.DCBridge, p.Roles["relay-spare"].Private)
	p.addLink("relay-spare", p.CallerBridge, p.Roles["relay-spare"].Public)
}

func (p Plan) relayBlackholeCommands(block bool) []Command {
	ns := p.Roles["relay"].Namespace
	var commands []Command
	for _, chain := range []string{"INPUT", "OUTPUT"} {
		rule := Command{"ip", "netns", "exec", ns, "iptables", "-w", "2"}
		if block {
			rule = append(rule, "-I", chain, "1")
		} else {
			rule = append(rule, "-D", chain)
		}
		commands = append(commands, append(rule, "-j", "DROP"))
	}
	return commands
}

// BlackholeRelay drops all traffic on the lost relay's public and private
// addresses, including established store connections. Processes keep running.
// The rules live only in the owned namespace and disappear with its cleanup.
func (t *Topology) BlackholeRelay(ctx context.Context, block bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("topology is closed")
	}
	return t.execute(ctx, t.Plan.relayBlackholeCommands(block))
}
