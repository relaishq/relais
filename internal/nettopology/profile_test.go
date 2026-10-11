package nettopology

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProfilePlanEndToEndDelayAndScope(t *testing.T) {
	p, err := NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	for _, name := range []string{"lan", "region", "far-caller", "lossy-dc"} {
		t.Run(name, func(t *testing.T) {
			profile, err := ParseProfile(name)
			require.NoError(t, err)
			settings, err := p.ProfilePlan(name)
			require.NoError(t, err)
			require.Len(t, settings, 2*len(p.Links))
			delays := map[string]time.Duration{}
			for _, s := range settings {
				require.True(t, OwnedLink(s.Interface, p.LinkPrefix))
				require.NotEqual(t, p.ManagementHost, s.Interface)
				require.NotEqual(t, p.ManagementPeer, s.Interface)
				require.True(t, s.Namespace == p.FabricNamespace || strings.HasPrefix(s.Namespace, p.ID+"-"))
				c := s.command()
				require.Equal(t, []string{"ip", "netns", "exec", s.Namespace, "tc"}, []string(c[:5]))
				require.Equal(t, s.Interface, c[8])
				delays[s.Role] += s.Delay
				if name == "lan" {
					require.Zero(t, s.Delay)
					require.Zero(t, s.Loss)
					require.Zero(t, s.Reorder)
				}
			}
			// Round trip traverses both directions of each endpoint veth. The
			// public relay endpoint must add no second copy of caller impairment.
			require.Equal(t, profile.CallerRTT, delays["caller"])
			dcRelay := time.Duration(0)
			for _, s := range settings {
				for _, l := range p.Links {
					if l.Role == "relay" && l.Bridge == p.DCBridge && (s.Interface == l.HostLink || s.Interface == l.PeerLink) {
						dcRelay += s.Delay
					}
				}
			}
			require.Equal(t, profile.DCRTT, dcRelay+delays["worker-0"])
			require.Equal(t, profile.DCRTT, delays["worker-0"]+delays["store"])
			require.Equal(t, profile.DCRTT, delays["control"]+delays["worker-0"])
		})
	}
	_, err = ParseProfile("bad; tc qdisc del dev en0 root")
	require.Error(t, err)
	far, _ := p.ProfilePlan("far-caller")
	for _, s := range far {
		if s.Role == "caller" {
			require.Equal(t, 10*time.Millisecond, s.Jitter)
			require.Equal(t, 2.0, s.Loss)
		}
	}
	lossy, _ := p.ProfilePlan("lossy-dc")
	require.Contains(t, strings.Join(lossy[0].command(), " "), "loss 0.05% reorder 0.1%")
}

func TestProfileApplicationRecordsActualQdiscAndPropagatesFailures(t *testing.T) {
	p, _ := NewPlan(0, 1, "aabbccddee", 2)
	var calls []Command
	topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
		calls = append(calls, c)
		if c[6] == "show" {
			return "qdisc netem 8001: root delay 10ms loss 0.5%", nil
		}
		return "", nil
	}}
	settings, err := topology.ApplyProfile(context.Background(), "region")
	require.NoError(t, err)
	require.Len(t, settings, len(p.Links)*2)
	for _, s := range settings {
		require.NotEmpty(t, s.Qdisc)
	}
	for _, c := range calls {
		require.Equal(t, "netns", c[1])
		require.NotEmpty(t, c[3])
	}
	topology.runner = func(context.Context, Command) (string, error) { return "", errors.New("tc denied") }
	_, err = topology.ApplyProfile(context.Background(), "region")
	require.ErrorContains(t, err, "tc denied")
	topology.closed = true
	_, err = topology.ApplyProfile(context.Background(), "lan")
	require.ErrorContains(t, err, "closed")
}

func TestBarrierLossRemovalUsesSameOwnedNamespaceAndRule(t *testing.T) {
	p, _ := NewPlan(0, 1, "aabbccddee", 2)
	var calls []Command
	topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) { calls = append(calls, c); return "", nil }}
	remove, err := topology.BarrierLoss(context.Background())
	require.NoError(t, err)
	require.NoError(t, remove())
	require.Len(t, calls, 2)
	require.Equal(t, p.Roles["relay"].Namespace, calls[0][3])
	require.Equal(t, "-A", calls[0][7])
	require.Equal(t, "-D", calls[1][7])
	calls[0][7] = "-D"
	require.Equal(t, calls[0], calls[1])
}
