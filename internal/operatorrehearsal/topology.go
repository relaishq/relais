package operatorrehearsal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Deployment is disposable resource ownership, independent of the gate latch.
// P6 must defer Close immediately after NewDeployment, even if Boot fails.
type Deployment struct {
	Topology      *Topology
	Runner        Runner
	Diagnostics   io.Writer
	Work, Archive string
	Layout        CPULayout
	owner         string
	networkID     string
	containerIDs  map[string]string
	aclNegative   bool
	diagnosticMu  sync.Mutex
}

func NewDeployment(t *Topology, work, archive string, layout CPULayout, runner Runner, diagnostics io.Writer) (*Deployment, error) {
	if t == nil || runner == nil || !filepath.IsAbs(work) || !filepath.IsAbs(archive) {
		return nil, errors.New("invalid-deployment")
	}
	checked, err := Layout(layout.Allowed, layout.Caller)
	if err != nil {
		return nil, err
	}
	if CPUList(checked.Services) != CPUList(layout.Services) {
		return nil, errors.New("invalid-service-cpus")
	}
	if strings.ContainsAny(work+archive, ":\n\r,") {
		return nil, errors.New("invalid-mount-path")
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return nil, err
	}
	return &Deployment{Topology: t, Runner: runner, Diagnostics: diagnostics, Work: work, Archive: archive, Layout: layout, owner: hex.EncodeToString(token[:]), containerIDs: map[string]string{}}, nil
}
func (d *Deployment) command(ctx context.Context, label string, args []string, input []byte) (CommandResult, error) {
	// Independent boot steps can run concurrently; keep each diagnostic intact.
	r := d.Runner.Run(ctx, Command{Label: label, Args: args, Input: input})
	if r.Err == nil && r.Code != nil && *r.Code == 0 && !r.Truncated {
		return r, nil
	}
	d.printFailure(label, r)
	return r, errors.New("container-step-failed:" + label)
}
func (d *Deployment) printFailure(label string, r CommandResult) {
	d.diagnosticMu.Lock()
	defer d.diagnosticMu.Unlock()
	if d.Diagnostics != nil {
		_, _ = fmt.Fprintf(d.Diagnostics, "container step %s failed\n%s", label, d.Topology.diagnosticStreamsLimit(r, commandOutputLimit(label)))
	}
}
func (d *Deployment) runArgs(slot, ip, image string, media bool) []string {
	weight := "256"
	if media {
		weight = "1024"
	}
	return []string{"docker", "run", "-d", "--name", d.Topology.containerName(slot), "--label", containerOwnerKey + "=" + d.owner, "--cpuset-cpus", CPUList(d.Layout.Services), "--cpu-shares", weight, "--network", d.networkID, "--ip", ip, image}
}

// ContainerIDs returns only confirmed creation identities for the local adapter.
// Names are creation labels; later execution never resolves them again.
func (d *Deployment) ContainerIDs() map[string]string { return maps.Clone(d.containerIDs) }
func (d *Deployment) startContainer(ctx context.Context, slot, label string, args []string) error {
	r, err := d.command(ctx, label, args, nil)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(r.Stdout))
	if !dockerID.MatchString(id) {
		return errors.New("container-creation-unconfirmed")
	}
	d.containerIDs[slot] = id
	return nil
}
func (d *Deployment) execArgs(slot string, options []string, args ...string) ([]string, error) {
	id := d.containerIDs[slot]
	if !dockerID.MatchString(id) {
		return nil, errors.New("container-creation-unconfirmed")
	}
	command := append([]string{"docker", "exec"}, options...)
	return append(append(command, id), args...), nil
}
func insertBeforeImage(args, options []string) []string {
	return append(append(args[:len(args)-1:len(args)-1], options...), args[len(args)-1])
}
func (d *Deployment) shell(ctx context.Context, slot, label, script string) error {
	args, err := d.execArgs(slot, []string{"-i"}, "sh", "-eu")
	if err != nil {
		return err
	}
	_, err = d.command(ctx, label, args, []byte(script))
	return err
}
func (d *Deployment) wait(ctx context.Context, label string, args []string, ready func(string) bool) error {
	for {
		r := d.Runner.Run(ctx, Command{Label: label, Args: args})
		if r.Err == nil && r.Code != nil && *r.Code == 0 && !r.Truncated && ready(string(r.Stdout)) {
			return nil
		}
		select {
		case <-ctx.Done():
			d.printFailure(label, r)
			return errors.New("container-unready:" + label)
		case <-time.After(time.Second):
		}
	}
}

// Boot consumes a pre-fetched digest-bound archive. All downloads/builds finish
// before media. Template download.sh still validates and installs that archive.
func (d *Deployment) Boot(ctx context.Context) error {
	if d == nil || d.owner == "" || d.Runner == nil || d.Topology == nil {
		return errors.New("invalid-deployment")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	f, err := os.Open(d.Archive)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != d.Topology.Config.ArchiveSHA256 {
		return errors.New("archive-digest-mismatch")
	}
	// Refuse an existing workspace instead of overwriting secret bundles.
	if err = os.Mkdir(d.Work, 0700); err != nil {
		return err
	}
	ca, err := certificates(filepath.Join(d.Work, "tls"))
	if err != nil {
		return err
	}
	dockerfile, err := assets.ReadFile("assets/Dockerfile")
	if err != nil {
		return err
	}
	image := d.Topology.Config.Name + "-host"
	if _, err = d.command(ctx, "image", []string{"docker", "build", "--progress=plain", "-t", image, "-"}, dockerfile); err != nil {
		return err
	}
	network, err := d.command(ctx, "network", []string{"docker", "network", "create", "--subnet", d.Topology.Subnet, d.Topology.Config.Name}, nil)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(network.Stdout))
	if !dockerID.MatchString(id) {
		return errors.New("network-creation-unconfirmed")
	}
	d.networkID = id
	store := d.runArgs("store", StoreIP, "valkey/valkey:9.0.6", false)
	store = insertBeforeImage(store, []string{"--tmpfs", "/data:rw", "-v", filepath.Join(d.Work, "tls/store") + ":/tls:ro"})
	store = append(store, "valkey-server", "--port", "0", "--tls-port", fmt.Sprint(d.Topology.Config.StorePort), "--tls-cert-file", "/tls/server.crt", "--tls-key-file", "/tls/server.key", "--tls-ca-cert-file", "/tls/ca.crt", "--tls-auth-clients", "no", "--tls-cluster", "yes", "--cluster-enabled", "yes", "--cluster-announce-hostname", StoreNode, "--cluster-preferred-endpoint-type", "hostname", "--cluster-require-full-coverage", "yes", "--save", "", "--appendonly", "no")
	if err = d.startContainer(ctx, "store", "store", store); err != nil {
		return err
	}
	ready, stop := context.WithTimeout(ctx, time.Minute)
	defer stop()
	storePing, err := d.valkey("PING")
	if err != nil {
		return err
	}
	if err = d.wait(ready, "store-ready", storePing, func(s string) bool { return strings.TrimSpace(s) == "PONG" }); err != nil {
		return err
	}
	if err = d.storeOK(ctx, "CLUSTER", "ADDSLOTSRANGE", "0", "16383"); err != nil {
		return err
	}
	storeInfo, err := d.valkey("CLUSTER", "INFO")
	if err != nil {
		return err
	}
	if err = d.wait(ready, "store-cluster", storeInfo, func(s string) bool { return strings.Contains(s, "cluster_state:ok") }); err != nil {
		return err
	}
	for _, u := range d.Topology.Users {
		if err = d.storeOK(ctx, append([]string{"ACL", "SETUSER", u.Name, "reset"}, append(u.Rules, ">"+d.Topology.Secrets[u.Secret])...)...); err != nil {
			return err
		}
	}
	if err = d.checkWrongACL(ctx); err != nil {
		return err
	}
	// No management user remains available to the hosts, matching the template.
	if err = d.storeOK(ctx, "ACL", "SETUSER", "default", "reset"); err != nil {
		return err
	}
	timeArgs := insertBeforeImage(d.runArgs("time", TimeIP, image, false), []string{"--cap-drop", "SYS_TIME", "--entrypoint", "sh"})
	timeArgs = append(timeArgs, "-c", `printf 'local stratum 1\nallow `+d.Topology.Subnet+`\n' > /etc/chrony-reference.conf && exec chronyd -d -x -u root -f /etc/chrony-reference.conf`)
	if err = d.startContainer(ctx, "time", "time", timeArgs); err != nil {
		return err
	}
	timeReady, timeDone := context.WithTimeout(ctx, time.Minute)
	defer timeDone()
	timeTracking, err := d.execArgs("time", nil, "chronyc", "-n", "tracking")
	if err != nil {
		return err
	}
	if err = d.wait(timeReady, "time-ready", timeTracking, func(s string) bool { return strings.Contains(s, "Normal") }); err != nil {
		return err
	}
	// Hosts first acquire their sandbox and clocks, then boot together so the
	// template readiness checks can see their peers. No readiness bypass exists.
	for _, slot := range Slots {
		host := d.Topology.Hosts[slot]
		bundle, x := d.Topology.Bundle(slot)
		if x != nil {
			return x
		}
		bundle["store-ca.pem"] = ca
		bundle["chrony.conf"] = []byte("server " + TimeIP + " prefer iburst minpoll 0 maxpoll 0\ndriftfile /var/lib/chrony/drift\n")
		bundle["chronyd.conf"] = []byte(chronyDropIn)
		dir := filepath.Join(d.Work, "hosts", slot)
		if x = writeFiles(dir, bundle); x != nil {
			return x
		}
		for _, name := range []string{"template-read", "boot-signal"} {
			if x = os.Chmod(filepath.Join(dir, name), 0755); x != nil {
				return x
			}
		}
		args := insertBeforeImage(d.runArgs(slot, host.IP, image, slot != "shared"), []string{"--hostname", slot, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", "-e", "container=docker", "--add-host", StoreEndpoint + ":" + StoreIP, "--add-host", StoreNode + ":" + StoreIP, "-v", dir + ":/rehearsal:ro", "-v", d.Archive + ":/release.tar.gz:ro"})
		if x = d.startContainer(ctx, slot, "host-"+slot, args); x != nil {
			return x
		}
		systemdCtx, done := context.WithTimeout(ctx, 2*time.Minute)
		systemdArgs, x := d.execArgs(slot, nil, "sh", "-c", "systemctl is-system-running || test \"$(systemctl is-system-running)\" = degraded")
		if x != nil {
			done()
			return x
		}
		x = d.wait(systemdCtx, "systemd", systemdArgs, func(s string) bool { return strings.Contains(s, "running") || strings.Contains(s, "degraded") })
		done()
		if x != nil {
			return x
		}
		script, x := d.Topology.SetupScript(slot)
		if x != nil {
			return x
		}
		if x = d.shell(ctx, slot, "setup-"+slot, script); x != nil {
			return x
		}
	}
	proxy := insertBeforeImage(d.runArgs("signal-front", SignalIP, image, false), []string{"-v", filepath.Join(d.Work, "tls/signal") + ":/tls:ro", "--entrypoint", "socat"})
	proxy = append(proxy, "openssl-listen:443,reuseaddr,fork,cert=/tls/server.pem,verify=0", "openssl:"+d.Topology.Hosts["shared"].IP+":24743,verify=0")
	if err = d.startContainer(ctx, "signal-front", "signal-front", proxy); err != nil {
		return err
	}
	errorsBySlot := make(chan error, len(Slots))
	var group sync.WaitGroup
	for _, slot := range Slots {
		group.Go(func() {
			args, x := d.execArgs(slot, []string{"-e", "RELAIS_TEST_ARCHIVE_URL=file:///release.tar.gz"}, "bash", "/rehearsal/user-data.sh")
			if x == nil {
				_, x = d.command(ctx, "boot-"+slot, args, nil)
			}
			if x == nil {
				script, e := d.Topology.VerifyScript(slot)
				x = e
				if x == nil {
					x = d.shell(ctx, slot, "verify-"+slot, script)
				}
			}
			errorsBySlot <- x
		})
	}
	group.Wait()
	close(errorsBySlot)
	for x := range errorsBySlot {
		if x != nil {
			return x
		}
	}
	return nil
}

// The disposable store still has its setup-only default user here. Install an
// actually wrong worker ACL, prove that the same authenticated EVAL fails with
// NOPERM, then restore the exact template rules and prove it succeeds again.
// Only after this does Boot disable the default user and start any host unit.
func (d *Deployment) checkWrongACL(ctx context.Context) error {
	var worker StoreUser
	for _, u := range d.Topology.Users {
		if u.Name == "worker" {
			worker = u
		}
	}
	if worker.Secret == "" {
		return errors.New("missing-worker-acl")
	}
	probe := func(label, expected string) error {
		r, err := d.storeCommand(ctx, label, &worker, "EVAL", "return 1", "0")
		if err != nil {
			return err
		}
		if expected == "NOPERM" && strings.Contains(string(r.Stdout), expected) || strings.TrimSpace(string(r.Stdout)) == expected {
			return nil
		}
		return errors.New("acl-control-unexpected-result")
	}
	if err := probe("acl-positive", "1"); err != nil {
		return err
	}
	if err := d.storeOK(ctx, "ACL", "SETUSER", worker.Name, "reset", "on", "-@all", "+ping", ">"+d.Topology.Secrets[worker.Secret]); err != nil {
		return err
	}
	if err := probe("negative-wrong-acl", "NOPERM"); err != nil {
		return err
	}
	args := append([]string{"ACL", "SETUSER", worker.Name, "reset"}, worker.Rules...)
	if err := d.storeOK(ctx, append(args, ">"+d.Topology.Secrets[worker.Secret])...); err != nil {
		return err
	}
	if err := probe("acl-positive", "1"); err != nil {
		return err
	}
	d.aclNegative = true
	return nil
}
func (d *Deployment) valkey(args ...string) ([]string, error) {
	return d.execArgs("store", []string{"-i"}, append([]string{"valkey-cli", "--raw", "--tls", "--cacert", "/tls/ca.crt", "-h", "127.0.0.1", "--sni", StoreEndpoint, "-p", fmt.Sprint(d.Topology.Config.StorePort)}, args...)...)
}

// Valkey's stdin parser accepts double-quoted arguments, including each ACL
// selector as one token. Authentication and ACL passwords never enter argv.
func storeLine(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ") + "\n"
}
func (d *Deployment) storeCommand(ctx context.Context, label string, user *StoreUser, args ...string) (CommandResult, error) {
	input := ""
	if user != nil {
		input = storeLine("AUTH", user.Name, d.Topology.Secrets[user.Secret])
	}
	input += storeLine(args...)
	command, err := d.valkey()
	if err != nil {
		return CommandResult{}, err
	}
	r, err := d.command(ctx, label, command, []byte(input))
	if err == nil && user != nil {
		remaining, ok := strings.CutPrefix(string(r.Stdout), "OK\n")
		if !ok {
			return r, errors.New("store-authentication-unconfirmed")
		}
		r.Stdout = []byte(remaining)
	}
	return r, err
}

func (d *Deployment) storeOK(ctx context.Context, args ...string) error {
	r, err := d.storeCommand(ctx, "store-admin", nil, args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(r.Stdout)) != "OK" {
		return errors.New("store-command-not-ok")
	}
	return nil
}

const containerOwnerKey = "relais.rehearsal.container-owner"

var dockerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Close selects containers only by their explicit per-deployment label. Images
// never carry that label. Networks use the exact confirmed creation ID. The
// store uses tmpfs, so cleanup need not delete any volume or image.
func (d *Deployment) Close(ctx context.Context) error {
	if d == nil || d.owner == "" {
		return errors.New("cleanup-ownership-missing")
	}
	var failures []error
	r, err := d.command(ctx, "cleanup-list", []string{"docker", "ps", "-aq", "--no-trunc", "--filter", "label=" + containerOwnerKey + "=" + d.owner}, nil)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, id := range strings.Fields(string(r.Stdout)) {
			if !dockerID.MatchString(id) {
				failures = append(failures, errors.New("invalid-cleanup-id"))
				continue
			}
			if _, err = d.command(ctx, "cleanup-container", []string{"docker", "rm", "-f", id}, nil); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if d.networkID != "" {
		if _, err = d.command(ctx, "cleanup-network", []string{"docker", "network", "rm", d.networkID}, nil); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// DetectLayout reads the executing Linux process's allowed mask. P6 records
// lscpu topology and caller/service affinities alongside this returned layout.
func DetectLayout(caller []int) (CPULayout, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return CPULayout{}, errors.New("linux-affinity-unavailable")
	}
	for _, line := range strings.Split(string(b), "\n") {
		if value, yes := strings.CutPrefix(line, "Cpus_allowed_list:"); yes {
			cpus, x := ParseCPUList(value)
			if x != nil {
				return CPULayout{}, x
			}
			return Layout(cpus, caller)
		}
	}
	return CPULayout{}, errors.New("linux-affinity-unavailable")
}
