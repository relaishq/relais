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
	recorded := map[string]string{}
	topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
		calls = append(calls, c)
		if c[6] == "show" {
			if value, ok := recorded[c[8]]; ok {
				return value, nil
			}
			return "qdisc noqueue 0: root", nil
		}
		if c[6] == "replace" {
			recorded[c[8]] = "qdisc netem 8001: root " + strings.Join(c[11:], " ")
		}
		if c[6] == "del" {
			recorded[c[8]] = "qdisc noqueue 0: root"
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

func TestProfileBarrierLossUsesWorkerIngressAndSameRemovalRules(t *testing.T) {
	p, err := NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	var calls []Command
	topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
		calls = append(calls, append(Command(nil), c...))
		return "", nil
	}}
	remove, err := topology.BarrierLoss(context.Background())
	require.NoError(t, err)
	// OUTPUT DROP returns EPERM to sendto; loss must occur on the receiving
	// worker's INPUT path, after transmission over the private veth.
	require.Equal(t, "INPUT", calls[0][8])
	require.Len(t, calls, 2)
	for i, c := range calls {
		role := []string{"worker-0", "worker-1"}[i]
		require.Equal(t, p.Roles[role].Namespace, c[3])
		require.Equal(t, "-A", c[7])
		var peer string
		for _, link := range p.Links {
			if link.Role == role {
				peer = link.PeerLink
			}
		}
		require.Equal(t, []string{"-i", peer, "-s", p.Roles["relay"].Private.String(), "-p", "udp", "-m", "u32", "--u32", "0>>22&0x3C@8=0x524c5301", "-j", "DROP"}, []string(c[9:]))
	}
	require.NoError(t, remove())
	require.Len(t, calls, 4)
	for i := 0; i < 2; i++ {
		want := append(Command(nil), calls[i]...)
		want[7] = "-D"
		require.Equal(t, want, calls[3-i])
	}
	require.NoError(t, remove())
	require.Len(t, calls, 4, "successful removal must be idempotent")
}

func TestProfileBarrierLossRollsBackPartialInstallationWithFreshContext(t *testing.T) {
	p, err := NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installErr, cleanupErr := errors.New("install denied"), errors.New("cleanup denied")
	var calls []Command
	topology := &Topology{Plan: p, runner: func(commandCtx context.Context, c Command) (string, error) {
		calls = append(calls, append(Command(nil), c...))
		if len(calls) == 2 {
			cancel()
			return "", installErr
		}
		if c[7] == "-D" {
			require.NoError(t, commandCtx.Err())
			_, bounded := commandCtx.Deadline()
			require.True(t, bounded)
			return "", cleanupErr
		}
		return "", nil
	}}
	remove, err := topology.BarrierLoss(ctx)
	require.Nil(t, remove)
	require.ErrorIs(t, err, installErr)
	require.ErrorIs(t, err, cleanupErr)
	require.Len(t, calls, 3)
	want := append(Command(nil), calls[0]...)
	want[7] = "-D"
	require.Equal(t, want, calls[2])
	topology.closed = true
	_, err = topology.BarrierLoss(context.Background())
	require.ErrorContains(t, err, "closed")
	require.Len(t, calls, 3)
}

func TestProfileBarrierLossRemovalTriesAllRulesAndRetriesFailures(t *testing.T) {
	p, err := NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deleted []Command
	cleanupErr := errors.New("cleanup denied")
	topology := &Topology{Plan: p, runner: func(commandCtx context.Context, c Command) (string, error) {
		if c[7] == "-D" {
			require.NoError(t, commandCtx.Err())
			deleted = append(deleted, c)
			if len(deleted) == 1 {
				return "", cleanupErr
			}
		}
		return "", nil
	}}
	remove, err := topology.BarrierLoss(ctx)
	require.NoError(t, err)
	cancel()
	require.ErrorIs(t, remove(), cleanupErr)
	require.Len(t, deleted, 2, "one cleanup failure must not skip the other worker")
	require.NoError(t, remove())
	require.Len(t, deleted, 3)
	require.Equal(t, deleted[0], deleted[2], "retry only the failed deletion")
	topology.closed = true
	topology.closeErr = cleanupErr
	require.ErrorIs(t, remove(), cleanupErr)
	require.Len(t, deleted, 3)
}

func TestProfileAppliesImpairmentsWithoutDelayAndVerifiesLoss(t *testing.T) {
	p, err := NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	plan, err := p.ProfilePlan("lan")
	require.NoError(t, err)
	for _, tc := range []struct {
		name          string
		loss          float64
		jitter, delay time.Duration
		reorder       float64
	}{
		{name: "loss only", loss: .5}, {name: "jitter only", jitter: time.Millisecond}, {name: "reorder only", reorder: .1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := plan[0]
			link.Loss, link.Jitter, link.Delay, link.Reorder = tc.loss, tc.jitter, tc.delay, tc.reorder
			var calls []Command
			topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
				calls = append(calls, c)
				if c[6] == "show" {
					return "qdisc netem 8001: root " + strings.Join(link.command()[11:], " "), nil
				}
				return "", nil
			}}
			applied, err := topology.applyProfileSettings(context.Background(), []LinkSetting{link})
			require.NoError(t, err)
			require.Len(t, applied, 1)
			require.Len(t, calls, 2)
			require.Equal(t, "replace", calls[0][6])
			require.Equal(t, "show", calls[1][6])
		})
	}
	for _, qdisc := range []string{"qdisc netem 8001: root delay 1ms", "qdisc netem 8001: root loss 0.1%", "qdisc netem 8001: root loss bad%", "qdisc noqueue 0: root"} {
		link := plan[0]
		link.Loss = .5
		link.Qdisc = qdisc
		require.Error(t, link.VerifyQdisc(), qdisc)
	}
	link := plan[0]
	link.Loss = .5
	link.Qdisc = "qdisc netem 8001: root loss random 0.500000%"
	require.NoError(t, link.VerifyQdisc())
}
