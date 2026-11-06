package clockprobe

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Target is the fixed digit used in the probe log.
type Target uint8

const (
	Control Target = iota
	WorkerOne
	WorkerTwo
	capacityTarget
)

func (t Target) String() string {
	switch t {
	case Control:
		return "control"
	case WorkerOne:
		return "w1"
	case WorkerTwo:
		return "w2"
	default:
		return "unknown"
	}
}

// Peer binds a fixed target to its validated private origin.
type Peer struct {
	Target Target
	Origin string
}

// Targets implements flag.Value, validating each explicit target once.
type Targets []Peer

func (t *Targets) String() string {
	var entries []string
	for _, p := range *t {
		entries = append(entries, p.Target.String()+"="+p.Origin)
	}
	return strings.Join(entries, ",")
}

func (t *Targets) Set(value string) error {
	name, origin, ok := strings.Cut(value, "=")
	var target Target
	switch name {
	case "control":
		target = Control
	case "w1":
		target = WorkerOne
	case "w2":
		target = WorkerTwo
	default:
		return fmt.Errorf("clock probe target must be control, w1 or w2")
	}
	if !ok {
		return fmt.Errorf("clock probe target requires name=origin")
	}
	for _, p := range *t {
		if p.Target == target {
			return fmt.Errorf("duplicate clock probe target")
		}
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(origin, "#") {
		return fmt.Errorf("clock probe target requires a private HTTP(S) origin without credentials, path, query or fragment")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || (!ip.IsPrivate() && !ip.IsLoopback()) || ip.Zone() != "" {
		return fmt.Errorf("clock probe target requires a literal private or loopback IP")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid clock probe target port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("invalid clock probe target port")
	}
	u.Path = ""
	*t = append(*t, Peer{Target: target, Origin: u.String()})
	return nil
}

// Validate requires a complete set only when recording is enabled.
func (t Targets) Validate(enabled bool, path string) error {
	if !enabled {
		if len(t) != 0 || path != "" {
			return fmt.Errorf("clock probe targets and file require -clock-prober")
		}
		return nil
	}
	if len(t) != 3 || path == "" {
		return fmt.Errorf("-clock-prober requires control, w1 and w2 targets and -clock-probe-file")
	}
	return nil
}
