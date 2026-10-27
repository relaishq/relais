package operatorrehearsal

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

//go:embed assets/*
var assets embed.FS

type Shim struct{ Name, Purpose string }

// Shims lists the entire substitution scope. None implements a cloud API.
func Shims() []Shim {
	return []Shim{
		{"template-metadata", "Read BootFiles and this host's metadata from local files; refuse other resources."},
		{"secret-source", "Read only secret files allowed by this host's template role; refuse other secrets."},
		{"boot-signal", "Record only this host's template boot exit status."},
		{"management-unit", "No-op service at the template management-unit path; host steps use docker exec."},
		{"host-execution", "One bounded docker exec on a captured host; no retry; nice 10."},
		{"time-reference", "One local chrony source; chronyd -x never changes the runner clock."},
		{"shared-root-mount", "Make only the container root mount shared for systemd credential delivery."},
		{"store-endpoint", "TLS-only single-node cluster with the template ACLs on a dedicated port; no durability or failover claim."},
		{"signaling-front", "Local TLS proxy to the template signal unit; no load-balancer or firewall claim."},
		{"relay-address", "Relay public address equals its container address; no address translation."},
		{"cpu-layout", "Two allowed CPUs for caller and decoder; remaining CPUs for services and driver."},
		{"no-instance-metadata", "Instance metadata is absent; there is no metadata emulator."},
	}
}

func (t *Topology) Bundle(slot string) (map[string][]byte, error) {
	h, ok := t.Hosts[slot]
	if !ok {
		return nil, errors.New("unknown-host")
	}
	scope, err := json.Marshal(struct {
		Stack    string   `json:"stack"`
		Region   string   `json:"region"`
		Resource string   `json:"resource"`
		Metadata []string `json:"metadata"`
		Secrets  []string `json:"secrets"`
	}{t.Config.Name, localRegion, h.Resource, []string{"BootFiles", h.Resource}, h.Secrets})
	if err != nil {
		return nil, err
	}
	common, err := json.Marshal(t.Common)
	if err != nil {
		return nil, err
	}
	metadata, err := json.Marshal(h.Metadata)
	if err != nil {
		return nil, err
	}
	read, err := assets.ReadFile("assets/template-read.py")
	if err != nil {
		return nil, err
	}
	signal, err := assets.ReadFile("assets/boot-signal.py")
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{"scope.json": scope, "metadata/BootFiles.json": common, "metadata/" + h.Resource + ".json": metadata, "template-read": read, "boot-signal": signal, "user-data.sh": []byte(h.UserData)}
	for _, id := range h.Secrets {
		out["secrets/"+id] = []byte(t.Secrets[id])
	}
	return out, nil
}

type Unit struct {
	Name, User                    string
	Sources, Sandbox, Credentials []string
}

func (t *Topology) Units(slot string) ([]Unit, error) {
	h, ok := t.Hosts[slot]
	if !ok {
		return nil, errors.New("unknown-host")
	}
	var node struct {
		Components []string `json:"components"`
	}
	if json.Unmarshal([]byte(h.Metadata.Files["/etc/relais/node.json"].Content), &node) != nil || len(node.Components) == 0 {
		return nil, errors.New("invalid-node-components")
	}
	var units []Unit
	for _, c := range node.Components {
		names := []string{"relais-" + c + ".service"}
		if c == "relay" {
			names = append(names, "relais-relay-standby.service")
		}
		for _, name := range names {
			f, exists := t.Common.Files["/etc/systemd/system/"+name]
			if !exists {
				return nil, errors.New("missing-template-unit")
			}
			u := Unit{Name: name}
			section := ""
			for _, line := range strings.Split(f.Content, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") {
					section = line
					continue
				}
				if section != "[Service]" {
					continue
				}
				key, value, found := strings.Cut(line, "=")
				if !found {
					continue
				}
				switch key {
				case "User":
					u.User = value
				case "EnvironmentFile":
					u.Sources = append(u.Sources, strings.TrimPrefix(value, "-"))
				case "LoadCredential":
					_, source, yes := strings.Cut(value, ":")
					if !yes {
						return nil, errors.New("invalid-template-credential")
					}
					u.Credentials = append(u.Credentials, value)
					u.Sources = append(u.Sources, source)
				case "Type", "Group", "ExecStart", "Restart", "TimeoutStopSec", "StandardOutput", "StandardError":
				default:
					u.Sandbox = append(u.Sandbox, line)
				}
			}
			if u.User == "" || len(u.Sources) == 0 {
				return nil, errors.New("invalid-template-unit")
			}
			units = append(units, u)
		}
	}
	return units, nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func CredentialProbe(units []Unit) (string, error) {
	if len(units) == 0 {
		return "", errors.New("missing-probe-unit")
	}
	u := units[0]
	for _, x := range units {
		if len(x.Credentials) > 0 {
			u = x
			break
		}
	}
	args := []string{"systemd-run --unit=relais-credential-probe --wait --pipe --quiet --collect -p User=nobody -p LoadCredential=probe:/run/rehearsal-probe/credential"}
	for _, setting := range u.Sandbox {
		args = append(args, "-p "+quote(setting))
	}
	args = append(args, `sh -c 'stat -c "%n %U:%G %a" "$CREDENTIALS_DIRECTORY" "$CREDENTIALS_DIRECTORY/probe"; test -r "$CREDENTIALS_DIRECTORY/probe"'`)
	return "install -d -m 0700 /run/rehearsal-probe\nprintf probe > /run/rehearsal-probe/credential\nchmod 0600 /run/rehearsal-probe/credential\n" + strings.Join(args, " ") + "\nrm -rf /run/rehearsal-probe\n", nil
}

const sharedRoot = "mount --make-rshared /\nfindmnt -no PROPAGATION / | grep -qw shared\n"
const chronyDropIn = "[Service]\nEnvironmentFile=\nEnvironment=\nExecStartPre=\nExecStartPost=\nExecStart=\nExecStart=/usr/sbin/chronyd -x -F 2 -f /etc/chrony.conf\n"

// ClockCheck requires exactly one source, selected from the expected reference,
// plus the running daemon's -x argument. Merely seeing the address is insufficient.
func ClockCheck() string { return clockCheck(true) }
func clockCheck(selected bool) string {
	check := `python3 - <<'PY'
import subprocess
from pathlib import Path
pid = subprocess.check_output(['systemctl', 'show', '-p', 'MainPID', '--value', 'chronyd'], text=True).strip()
args = Path('/proc/' + pid + '/cmdline').read_bytes().split(b'\0')
sources = subprocess.check_output(['chronyc', '-n', 'sources'], text=True).splitlines()
tracking = subprocess.check_output(['chronyc', '-n', 'tracking'], text=True)
rows = [s.split() for s in sources if s.startswith(('^', '=', '#'))]
if b'-x' not in args or len(rows) != 1 or len(rows[0]) < 2:
    raise SystemExit('local time source check failed')
if rows[0][1] != '` + TimeIP + `':
    print('wrong-time-source')
    raise SystemExit(42)
`
	if selected {
		check += `if rows[0][0] != '^*':
    raise SystemExit('local time reference is not selected')
if not any(s.strip() == 'Leap status     : Normal' for s in tracking.splitlines()):
    raise SystemExit('local time tracking check failed')
`
	}
	return check + "PY\n"
}

func (t *Topology) SetupScript(slot string) (string, error) {
	units, err := t.Units(slot)
	if err != nil {
		return "", err
	}
	probe, err := CredentialProbe(units)
	if err != nil {
		return "", err
	}
	return "set -eu\n" + `ln -s /rehearsal/template-read /usr/bin/aws
install -d /opt/aws/bin
ln -s /rehearsal/boot-signal /opt/aws/bin/cfn-signal
install -m 0644 /rehearsal/store-ca.pem /etc/pki/ca-trust/source/anchors/relais-rehearsal.pem
update-ca-trust
systemctl mask chrony-config.service systemd-networkd.service systemd-networkd.socket systemd-networkd-wait-online.service
install -m 0644 /rehearsal/chrony.conf /etc/chrony.conf
install -d /etc/systemd/system/chronyd.service.d
ln -sf /dev/null /etc/systemd/system/chronyd.service.d/default-sources.conf
install -m 0644 /rehearsal/chronyd.conf /etc/systemd/system/chronyd.service.d/zz-rehearsal.conf
systemctl daemon-reload
systemctl enable chronyd
systemctl restart chronyd
sleep 3
` + // Fast source presence pre-check, before the longer synchronization wait.
		clockCheck(false) + "chronyc -n waitsync 240 0 20 1\n" + ClockCheck() + sharedRoot + probe, nil
}

func (t *Topology) VerifyScript(slot string) (string, error) {
	units, err := t.Units(slot)
	if err != nil {
		return "", err
	}
	s := "set -eu\ntest \"$(cat /var/lib/rehearsal/boot-status)\" = 0\n"
	for _, u := range units {
		s += "systemctl is-active --quiet " + quote(u.Name) + "\n"
		for _, source := range u.Sources {
			s += "test -s " + quote(source) + "\nstat -c '%n %U:%G %a' " + quote(source) + "\n"
		}
	}
	return s + ClockCheck(), nil
}

// CPU lists are actual allowed IDs, never an assumption that 0..N are allowed.
type CPULayout struct{ Allowed, Caller, Services []int }

func Layout(allowed, caller []int) (CPULayout, error) {
	allowed = append([]int(nil), allowed...)
	caller = append([]int(nil), caller...)
	if len(allowed) < 4 {
		return CPULayout{}, errors.New("rehearsal-needs-four-allowed-cpus")
	}
	seen := map[int]bool{}
	for _, cpu := range allowed {
		if cpu < 0 || seen[cpu] {
			return CPULayout{}, errors.New("invalid-cpu-mask")
		}
		seen[cpu] = true
	}
	if len(caller) == 0 {
		caller = append(caller, allowed[:2]...)
	}
	if len(caller) != 2 || caller[0] == caller[1] || !seen[caller[0]] || !seen[caller[1]] {
		return CPULayout{}, errors.New("invalid-caller-cpus")
	}
	services := []int{}
	for _, cpu := range allowed {
		if cpu != caller[0] && cpu != caller[1] {
			services = append(services, cpu)
		}
	}
	return CPULayout{allowed, caller, services}, nil
}
func CPUList(cpus []int) string {
	values := make([]string, len(cpus))
	for i, cpu := range cpus {
		values[i] = strconv.Itoa(cpu)
	}
	return strings.Join(values, ",")
}
func ParseCPUList(text string) ([]int, error) {
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(text), ",") {
		first, last, rangeOK := strings.Cut(part, "-")
		lo, err := strconv.Atoi(first)
		if err != nil || lo < 0 {
			return nil, errors.New("invalid-cpu-list")
		}
		hi := lo
		if rangeOK {
			hi, err = strconv.Atoi(last)
			if err != nil || hi < lo || hi-lo > 65536 {
				return nil, errors.New("invalid-cpu-list")
			}
		}
		for cpu := lo; cpu <= hi; cpu++ {
			cpus = append(cpus, cpu)
		}
	}
	return cpus, nil
}
func DriverCommand(layout CPULayout, executable string, args ...string) []string {
	return append([]string{"taskset", "-c", CPUList(layout.Services), executable}, args...)
}
func (t *Topology) containerName(slot string) string {
	return fmt.Sprintf("%s-%s", t.Config.Name, slot)
}
