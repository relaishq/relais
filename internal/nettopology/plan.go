// Package nettopology owns the real-process driver's emulated Linux machines.
package nettopology

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

type Role struct {
	Namespace       string
	Private, Public netip.Addr
}

// Link is the per-interface profile seam. LAN leaves these links unshaped;
// later profiles can attach tc qdiscs to HostLink (in the fabric namespace)
// and PeerLink (in the role namespace) independently.
type Link struct {
	Role, Bridge, HostLink, PeerLink string
	Address                          netip.Prefix
}
type Plan struct {
	ID, LinkPrefix, DCBridge, CallerBridge          string
	FabricNamespace, ManagementHost, ManagementPeer string
	Datacentre, CallerNet                           netip.Prefix
	Host                                            netip.Addr
	Roles                                           map[string]Role
	Links                                           []Link
}

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{10}$`)
var ownedNamespace = regexp.MustCompile(`^relais-net-([0-9]+)-([1-9][0-9]*)-([0-9a-f]{10})-(relay|control|store|caller|fabric|worker-[0-9]+)$`)
var linkSuffix = regexp.MustCompile(`^[hp][0-9a-f]{2}$`)

// OwnedLink includes veths interrupted before either end enters a namespace.
func OwnedLink(name, prefix string) bool {
	if !strings.HasPrefix(prefix, "rn") || !tokenPattern.MatchString(strings.TrimPrefix(prefix, "rn")) || !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	return suffix == "d" || suffix == "c" || suffix == "mh" || suffix == "mp" || linkSuffix.MatchString(suffix)
}

func NewPlan(uid, pid int, token string, workers int) (Plan, error) {
	if uid < 0 || pid < 1 || !tokenPattern.MatchString(token) || workers < 1 || workers > 200 {
		return Plan{}, errors.New("invalid topology owner, token or worker count")
	}
	raw, _ := hex.DecodeString(token)
	dc := netip.MustParsePrefix(fmt.Sprintf("10.%d.%d.0/24", 128+raw[0]%64, raw[1]))
	public := netip.MustParsePrefix(fmt.Sprintf("10.%d.%d.0/24", 192+raw[0]%32, raw[1]))
	p := Plan{ID: fmt.Sprintf("relais-net-%d-%d-%s", uid, pid, token), LinkPrefix: "rn" + token, Datacentre: dc, CallerNet: public, Host: address(dc, 1), Roles: map[string]Role{}}
	p.DCBridge, p.CallerBridge = p.LinkPrefix+"d", p.LinkPrefix+"c"
	p.FabricNamespace = p.ID + "-fabric"
	p.ManagementHost, p.ManagementPeer = p.LinkPrefix+"mh", p.LinkPrefix+"mp"
	p.Roles["relay"] = Role{Namespace: p.ID + "-relay", Private: address(dc, 2), Public: address(public, 2)}
	p.Roles["control"] = Role{Namespace: p.ID + "-control", Private: address(dc, 3)}
	p.Roles["store"] = Role{Namespace: p.ID + "-store", Private: address(dc, 4)}
	p.Roles["caller"] = Role{Namespace: p.ID + "-caller", Public: address(public, 3)}
	names := []string{"relay", "control", "store"}
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("worker-%d", i)
		p.Roles[name] = Role{Namespace: p.ID + "-" + name, Private: address(dc, byte(10+i))}
		names = append(names, name)
	}
	for _, name := range names {
		p.addLink(name, p.DCBridge, p.Roles[name].Private)
	}
	p.addLink("relay", p.CallerBridge, p.Roles["relay"].Public)
	p.addLink("caller", p.CallerBridge, p.Roles["caller"].Public)
	return p, nil
}
func address(prefix netip.Prefix, last byte) netip.Addr {
	b := prefix.Addr().As4()
	b[3] = last
	return netip.AddrFrom4(b)
}
func (p *Plan) addLink(role, bridge string, ip netip.Addr) {
	i := len(p.Links)
	p.Links = append(p.Links, Link{Role: role, Bridge: bridge, HostLink: fmt.Sprintf("%sh%02x", p.LinkPrefix, i), PeerLink: fmt.Sprintf("%sp%02x", p.LinkPrefix, i), Address: netip.PrefixFrom(ip, 24)})
}
func (p Plan) PrivateNets() string { return p.Datacentre.String() }

// StaleOwner only recognizes exact driver namespace names for this UID.
// A living PID is never considered stale (including a reused PID).
func StaleOwner(name string, uid int, alive func(int) bool) (string, string, bool) {
	match := ownedNamespace.FindStringSubmatch(name)
	if match == nil || match[1] != strconv.Itoa(uid) {
		return "", "", false
	}
	pid, err := strconv.Atoi(match[2])
	if err != nil || alive(pid) {
		return "", "", false
	}
	return fmt.Sprintf("relais-net-%s-%s-%s", match[1], match[2], match[3]), "rn" + match[3], true
}

type Command []string

func (p Plan) SetupCommands() []Command {
	commands := []Command{{"ip", "netns", "add", p.FabricNamespace}, {"ip", "-n", p.FabricNamespace, "link", "set", "lo", "up"}}
	seen := map[string]bool{}
	for _, link := range p.Links {
		ns := p.Roles[link.Role].Namespace
		if !seen[ns] {
			commands = append(commands, Command{"ip", "netns", "add", ns}, Command{"ip", "-n", ns, "link", "set", "lo", "up"})
			if link.Role == "caller" || link.Role == "relay" {
				commands = append(commands, Command{"ip", "netns", "exec", ns, "sysctl", "-q", "-w", "net.ipv6.conf.all.disable_ipv6=1", "net.ipv6.conf.default.disable_ipv6=1"})
			}
			seen[ns] = true
		}
	}
	commands = append(commands,
		Command{"ip", "-n", p.FabricNamespace, "link", "add", p.DCBridge, "type", "bridge"},
		Command{"ip", "-n", p.FabricNamespace, "link", "set", p.DCBridge, "up"},
		Command{"ip", "-n", p.FabricNamespace, "link", "add", p.CallerBridge, "type", "bridge"},
		Command{"ip", "-n", p.FabricNamespace, "link", "set", p.CallerBridge, "up"},
		Command{"ip", "link", "add", p.ManagementHost, "type", "veth", "peer", "name", p.ManagementPeer},
		Command{"ip", "link", "set", p.ManagementPeer, "netns", p.FabricNamespace},
		Command{"ip", "-n", p.FabricNamespace, "link", "set", p.ManagementPeer, "master", p.DCBridge},
		Command{"ip", "-n", p.FabricNamespace, "link", "set", p.ManagementPeer, "up"},
		Command{"ip", "addr", "add", netip.PrefixFrom(p.Host, 24).String(), "dev", p.ManagementHost},
		Command{"ip", "link", "set", p.ManagementHost, "up"})
	for _, link := range p.Links {
		ns := p.Roles[link.Role].Namespace
		commands = append(commands,
			Command{"ip", "link", "add", link.HostLink, "type", "veth", "peer", "name", link.PeerLink},
			Command{"ip", "link", "set", link.PeerLink, "netns", ns},
			Command{"ip", "link", "set", link.HostLink, "netns", p.FabricNamespace},
			Command{"ip", "-n", p.FabricNamespace, "link", "set", link.HostLink, "master", link.Bridge},
			Command{"ip", "-n", p.FabricNamespace, "link", "set", link.HostLink, "up"},
			Command{"ip", "-n", ns, "addr", "add", link.Address.String(), "dev", link.PeerLink},
			Command{"ip", "-n", ns, "link", "set", link.PeerLink, "up"})
	}
	commands = append(commands, Command{"ip", "netns", "exec", p.Roles["relay"].Namespace, "sysctl", "-q", "-w", "net.ipv4.ip_forward=0"})
	for _, chain := range []string{"INPUT", "OUTPUT", "FORWARD"} {
		commands = append(commands, Command{"ip", "netns", "exec", p.Roles["caller"].Namespace, "iptables", "-w", "2", "-P", chain, "DROP"})
	}
	return commands
}
func (p Plan) MediaCommands(media netip.AddrPort) ([]Command, error) {
	if media.Addr() != p.Roles["relay"].Public || media.Port() == 0 || media.Port() == 6379 {
		return nil, errors.New("media endpoint must use the relay's caller-side address and a dedicated port")
	}
	ns := p.Roles["caller"].Namespace
	return []Command{
		{"ip", "netns", "exec", ns, "iptables", "-w", "2", "-F", "OUTPUT"},
		{"ip", "netns", "exec", ns, "iptables", "-w", "2", "-F", "INPUT"},
		{"ip", "netns", "exec", ns, "iptables", "-w", "2", "-A", "OUTPUT", "-p", "udp", "-d", media.Addr().String(), "--dport", strconv.Itoa(int(media.Port())), "-j", "ACCEPT"},
		{"ip", "netns", "exec", ns, "iptables", "-w", "2", "-A", "INPUT", "-p", "udp", "-s", media.Addr().String(), "--sport", strconv.Itoa(int(media.Port())), "-j", "ACCEPT"},
	}, nil
}
