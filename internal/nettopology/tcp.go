package nettopology

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

var TCPCounterNames = []string{"TcpRetransSegs", "TCPLossProbes", "TCPLossProbeRecovery", "TCPTimeouts", "TCPSynRetrans", "TCPFastRetrans", "TCPSpuriousRTOs"}

type TCPSnapshot struct {
	Role      string            `json:"role"`
	Namespace string            `json:"namespace"`
	Start     time.Time         `json:"start"`
	End       time.Time         `json:"end"`
	Counters  map[string]uint64 `json:"counters"`
	Missing   []string          `json:"missing,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// ParseTCPCounters pairs kernel header/value rows. Absent counters are missing,
// never fabricated as zero (kernel versions expose different counter sets).
func ParseTCPCounters(raw string) (map[string]uint64, []string, error) {
	counters := map[string]uint64{}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	for i := 0; i < len(lines); i++ {
		header := strings.Fields(lines[i])
		if len(header) == 0 || (header[0] != "Tcp:" && header[0] != "TcpExt:") {
			continue
		}
		i++
		if i >= len(lines) {
			return nil, nil, errors.New("missing TCP counter values")
		}
		values := strings.Fields(lines[i])
		if len(values) != len(header) || values[0] != header[0] {
			return nil, nil, errors.New("mismatched TCP counter header/values")
		}
		for j, name := range header[1:] {
			if header[0] == "Tcp:" {
				name = "Tcp" + name
			}
			for _, wanted := range TCPCounterNames {
				if name == wanted {
					value, err := strconv.ParseUint(values[j+1], 10, 64)
					if err != nil {
						return nil, nil, fmt.Errorf("%s: %w", name, err)
					}
					counters[name] = value
				}
			}
		}
	}
	var missing []string
	for _, name := range TCPCounterNames {
		if _, ok := counters[name]; !ok {
			missing = append(missing, name)
		}
	}
	return counters, missing, nil
}

// SnapshotTCP reads every owned namespace plus the driver namespace: lossy-dc
// routes workers through a driver-owned uncertainty proxy. These counters also
// include heartbeats, snapshots, media consent and metrics, not just move RPCs.
// Sampling is sequential; timestamps retain each namespace's actual window.
func (t *Topology) SnapshotTCP(ctx context.Context) []TCPSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	roles := make([]string, 0, len(t.Plan.Roles))
	for role := range t.Plan.Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	roles = append(roles, "fabric", "driver")
	snapshots := make([]TCPSnapshot, 0, len(roles))
	for _, role := range roles {
		ns := t.Plan.Roles[role].Namespace
		if role == "fabric" {
			ns = t.Plan.FabricNamespace
		}
		command := Command{"cat", "/proc/net/snmp", "/proc/net/netstat"}
		if ns != "" {
			command = append(Command{"ip", "netns", "exec", ns}, command...)
		}
		snapshot := TCPSnapshot{Role: role, Namespace: ns, Start: time.Now()}
		var raw string
		var err error
		if t.closed {
			err = errors.New("topology is closed")
		} else {
			raw, err = t.runner(ctx, command)
		}
		snapshot.End = time.Now()
		if err == nil {
			snapshot.Counters, snapshot.Missing, err = ParseTCPCounters(raw)
		}
		if err != nil {
			snapshot.Error = err.Error()
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

type PrivateRoute struct {
	Role      string        `json:"role"`
	Namespace string        `json:"namespace"`
	Interface string        `json:"interface"`
	Prefix    string        `json:"prefix"`
	RtoMin    time.Duration `json:"rto_min_ns,omitempty"`
	QuickACK  bool          `json:"quickack,omitempty"`
	Observed  string        `json:"observed"`
}

func (p Plan) PrivateRouteCommands() []Command {
	var commands []Command
	for _, link := range p.Links {
		if link.Bridge != p.DCBridge {
			continue
		}
		commands = append(commands, Command{"ip", "-n", p.Roles[link.Role].Namespace, "route", "change", p.Datacentre.String(), "dev", link.PeerLink, "src", p.Roles[link.Role].Private.String(), "rto_min", "5ms", "quickack", "1"})
	}
	return commands
}

// TunePrivateRoutes applies the recommended private TCP recovery policy to
// existing connected DC routes inside owned namespaces. Crash-run enables it
// by default. No host/caller route or sysctl changes; teardown removes it.
func (t *Topology) TunePrivateRoutes(ctx context.Context) ([]PrivateRoute, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("topology is closed")
	}
	if err := t.execute(ctx, t.Plan.PrivateRouteCommands()); err != nil {
		return nil, err
	}
	return t.privateRoutes(ctx, true)
}

// ObservePrivateRoutes reads the existing private routes without changing them.
// Absent explicit TCP metrics stay unspecified; Observed retains kernel output.
func (t *Topology) ObservePrivateRoutes(ctx context.Context) ([]PrivateRoute, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("topology is closed")
	}
	return t.privateRoutes(ctx, false)
}

func (t *Topology) privateRoutes(ctx context.Context, tuned bool) ([]PrivateRoute, error) {
	var settings []PrivateRoute
	for _, link := range t.Plan.Links {
		if link.Bridge != t.Plan.DCBridge {
			continue
		}
		ns := t.Plan.Roles[link.Role].Namespace
		observed, err := t.runner(ctx, Command{"ip", "-n", ns, "route", "show", t.Plan.Datacentre.String()})
		if err != nil {
			return nil, err
		}
		route := PrivateRoute{Role: link.Role, Namespace: ns, Interface: link.PeerLink, Prefix: t.Plan.Datacentre.String(), Observed: strings.TrimSpace(observed)}
		if tuned {
			if !strings.Contains(observed, "rto_min lock 5ms") || !strings.Contains(observed, "quickack 1") {
				return nil, fmt.Errorf("private route tuning not observed in %s: %s", ns, observed)
			}
			route.RtoMin, route.QuickACK = 5*time.Millisecond, true
		}
		settings = append(settings, route)
	}
	return settings, nil
}

func (t *Topology) KernelRelease(ctx context.Context) (string, error) {
	release, err := t.runner(ctx, Command{"uname", "-r"})
	return strings.TrimSpace(release), err
}
