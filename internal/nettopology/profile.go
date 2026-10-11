package nettopology

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Profile describes the total path. Each datacentre path crosses two endpoint
// veths; caller impairment is applied only to the caller veth, never twice.
type Profile struct {
	Name                    string
	CallerRTT, CallerJitter time.Duration
	CallerLoss              float64
	DCRTT, DCJitter         time.Duration
	DCLoss, Reorder         float64
	FreshnessCeiling        time.Duration
}

func ParseProfile(name string) (Profile, error) {
	p := Profile{Name: name, FreshnessCeiling: 50 * time.Millisecond}
	switch name {
	case "lan":
	case "region":
		p.CallerRTT, p.CallerLoss, p.DCRTT = 20*time.Millisecond, 0.5, 500*time.Microsecond
		p.FreshnessCeiling = 150 * time.Millisecond
	case "far-caller":
		p.CallerRTT, p.CallerJitter, p.CallerLoss, p.DCRTT = 150*time.Millisecond, 20*time.Millisecond, 2, 500*time.Microsecond
		p.FreshnessCeiling = 250 * time.Millisecond
	case "lossy-dc":
		p.DCRTT, p.DCJitter, p.DCLoss, p.Reorder = 1500*time.Microsecond, 500*time.Microsecond, 0.1, 0.1
	default:
		return Profile{}, fmt.Errorf("unknown network profile %q (lan, region, far-caller, lossy-dc)", name)
	}
	return p, nil
}

type LinkSetting struct {
	Role, Namespace, Interface string
	Delay, Jitter              time.Duration
	Loss, Reorder              float64
	Qdisc                      string
}

func (p Plan) ProfilePlan(name string) ([]LinkSetting, error) {
	profile, err := ParseProfile(name)
	if err != nil {
		return nil, err
	}
	var settings []LinkSetting
	for _, link := range p.Links {
		delay, jitter, loss, reorder := profile.DCRTT/4, profile.DCJitter/4, profile.DCLoss/2, profile.Reorder
		if link.Bridge == p.CallerBridge {
			delay, jitter, loss, reorder = 0, 0, 0, 0
			if link.Role == "caller" {
				delay, jitter, loss = profile.CallerRTT/2, profile.CallerJitter/2, profile.CallerLoss
			}
		}
		for _, end := range []struct{ ns, dev string }{{p.FabricNamespace, link.HostLink}, {p.Roles[link.Role].Namespace, link.PeerLink}} {
			settings = append(settings, LinkSetting{Role: link.Role, Namespace: end.ns, Interface: end.dev, Delay: delay, Jitter: jitter, Loss: loss, Reorder: reorder})
		}
	}
	return settings, nil
}
func (s LinkSetting) command() Command {
	c := Command{"ip", "netns", "exec", s.Namespace, "tc", "qdisc", "replace", "dev", s.Interface, "root", "netem", "limit", "10000", "delay", fmt.Sprintf("%dus", s.Delay.Microseconds())}
	if s.Jitter > 0 {
		c = append(c, fmt.Sprintf("%dus", s.Jitter.Microseconds()))
	}
	if s.Loss > 0 {
		c = append(c, "loss", fmt.Sprintf("%g%%", s.Loss))
	}
	if s.Reorder > 0 {
		c = append(c, "reorder", fmt.Sprintf("%g%%", s.Reorder))
	}
	return c
}

// ApplyProfile touches only exact interfaces in the owned plan. Deleting the
// topology's veths and namespaces also deletes every attached qdisc.
func (t *Topology) ApplyProfile(ctx context.Context, name string) ([]LinkSetting, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("topology is closed")
	}
	settings, err := t.Plan.ProfilePlan(name)
	if err != nil {
		return nil, err
	}
	for i := range settings {
		s := &settings[i]
		if s.Delay > 0 {
			if _, err := t.runner(ctx, s.command()); err != nil {
				return nil, err
			}
		}
		output, err := t.runner(ctx, Command{"ip", "netns", "exec", s.Namespace, "tc", "qdisc", "show", "dev", s.Interface})
		if err != nil {
			return nil, err
		}
		if s.Delay == 0 && strings.Contains(output, "netem") {
			if _, err := t.runner(ctx, Command{"ip", "netns", "exec", s.Namespace, "tc", "qdisc", "del", "dev", s.Interface, "root"}); err != nil {
				return nil, err
			}
			output, err = t.runner(ctx, Command{"ip", "netns", "exec", s.Namespace, "tc", "qdisc", "show", "dev", s.Interface})
			if err != nil {
				return nil, err
			}
		}
		s.Qdisc = strings.TrimSpace(output)
		if s.Delay > 0 && !strings.Contains(s.Qdisc, "netem") {
			return nil, fmt.Errorf("netem absent on %s/%s", s.Namespace, s.Interface)
		}
	}
	return settings, nil
}

// BarrierLoss installs a brief, driver-owned loss burst for the private drain
// marker, so a lossy-dc trial proves resend instead of hoping a 0.1% random
// loss happens to hit one of a handful of barriers. Media is not selected.
// Drop on worker ingress: OUTPUT DROP would return EPERM to the relay sender,
// unlike a real lossy link. Both workers are covered as ownership can change.
func (t *Topology) BarrierLoss(ctx context.Context) (func() error, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("topology is closed")
	}
	var installed []Command
	// The caller holds t.mu. Cleanup uses fresh contexts even when installation
	// was canceled, and retains failed deletions for a subsequent removal try.
	removeRules := func() error {
		var failures []error
		for i := len(installed) - 1; i >= 0; i-- {
			remove := append(Command(nil), installed[i]...)
			remove[7] = "-D"
			if _, err := t.cleanupCommand(remove); err != nil {
				failures = append(failures, err)
			} else {
				installed = append(installed[:i], installed[i+1:]...)
			}
		}
		return errors.Join(failures...)
	}
	for _, link := range t.Plan.Links {
		if !strings.HasPrefix(link.Role, "worker-") {
			continue
		}
		ns := t.Plan.Roles[link.Role].Namespace
		rule := Command{"ip", "netns", "exec", ns, "iptables", "-w", "2", "-A", "INPUT", "-i", link.PeerLink, "-s", t.Plan.Roles["relay"].Private.String(), "-p", "udp", "-m", "u32", "--u32", "0>>22&0x3C@8=0x524c5301", "-j", "DROP"}
		if _, err := t.runner(ctx, rule); err != nil {
			return nil, errors.Join(err, removeRules())
		}
		installed = append(installed, rule)
	}
	return func() error {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			// Close owns namespace cleanup; preserve any teardown failure.
			return t.closeErr
		}
		return removeRules()
	}, nil
}
