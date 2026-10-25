//go:build !windows

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

type pairProc struct {
	Name, Role string
	Release    *pairRelease
	Child      *clusterprocess.Child
	Ready      clusterprocess.Ready
}

// pairTopology is the lean process shape on loopback: relay plus same-host
// standby, two legacy echo workers, control and signaling on one Redis.
type pairTopology struct {
	manager                                                  *clusterprocess.Manager
	dir, token                                               string
	env                                                      []string
	callCap                                                  int
	settle                                                   time.Duration
	relayMedia, relayLeg, relayHTTP, controlAddr, signalAddr string
	workerHTTP, workerMedia                                  [2]string
	workers                                                  [2]*pairProc
	relay, standby, control, signal                          *pairProc
	launches, relays, checks                                 int
	calls                                                    []*callharness.Call
	relayWindows                                             []pairWindow
	observations                                             []pairObservation
	detail                                                   string
	client                                                   *http.Client
}

func pairWorkerName(i int) string { return "worker-" + strconv.Itoa(i) }

func newPairTopology(manager *clusterprocess.Manager, dir string, env []string, token string, callCap int, settle time.Duration) (*pairTopology, error) {
	t := &pairTopology{manager: manager, dir: dir, env: env, token: token, callCap: callCap, settle: settle, client: &http.Client{Timeout: 5 * time.Second}}
	var err error
	// Fixed addresses let every replacement rejoin the same endpoint.
	for _, target := range []*string{&t.relayHTTP, &t.controlAddr, &t.signalAddr, &t.workerHTTP[0], &t.workerHTTP[1]} {
		if *target, err = clusterprocess.FreeTCP(); err != nil {
			return nil, err
		}
	}
	for _, target := range []*string{&t.relayMedia, &t.relayLeg, &t.workerMedia[0], &t.workerMedia[1]} {
		if *target, err = clusterprocess.FreeUDP(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *pairTopology) launch(name, role string, release *pairRelease, env []string, args ...string) (*pairProc, error) {
	t.launches++
	c, err := t.manager.Start(t.dir, fmt.Sprintf("%02d-%s-%s", t.launches, name, release.Version), env, args...)
	if err != nil {
		return nil, fmt.Errorf("%s launch: %w", name, err)
	}
	return &pairProc{Name: name, Role: role, Release: release, Child: c}, nil
}

func (t *pairTopology) ready(ctx context.Context, p *pairProc, timeout time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := p.Child.Ready(wait)
	if err != nil {
		return fmt.Errorf("%s readiness: %w", p.Name, err)
	}
	p.Ready = r
	return nil
}

// stop sends SIGTERM and waits; it never escalates, so an unclean exit is evidence.
func (t *pairTopology) stop(ctx context.Context, p *pairProc, timeout time.Duration) error {
	if err := p.Child.SignalGroup(syscall.SIGTERM); err != nil {
		return fmt.Errorf("%s stop: %w", p.Name, err)
	}
	select {
	case <-p.Child.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return fmt.Errorf("%s did not exit within %s of SIGTERM", p.Name, timeout)
	}
}

func (t *pairTopology) pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (t *pairTopology) relayArgs(r *pairRelease, standby bool) []string {
	args := []string{r.binary("relais-relay"), "-media", t.relayMedia, "-leg", t.relayLeg, "-http", t.relayHTTP, "-relay-lease", "preview"}
	if standby {
		args = append(args, "-standby")
	}
	return args
}

func (t *pairTopology) workerArgs(r *pairRelease, i int) []string {
	return []string{r.binary("relais-worker"), "-name", strconv.Itoa(i), "-media", t.workerMedia[i], "-http", t.workerHTTP[i], "-relay-leg", t.relayLeg, "-relay-media", t.relayMedia, "-control", "http://" + t.controlAddr, "-agent", "echo"}
}

func (t *pairTopology) controlArgs(r *pairRelease) []string {
	args := []string{r.binary("relais-control"), "-http", t.controlAddr, "-relay", "http://" + t.relayHTTP, "-worker-call-cap", strconv.Itoa(t.callCap)}
	for i, addr := range t.workerHTTP {
		args = append(args, "-worker", strconv.Itoa(i)+"=http://"+addr)
	}
	return args
}

func (t *pairTopology) startRelay(ctx context.Context, r *pairRelease) error {
	t.relays++
	p, err := t.launch("relay-"+strconv.Itoa(t.relays), "relay", r, withoutKey(t.env), t.relayArgs(r, false)...)
	if err != nil {
		return err
	}
	if err := t.ready(ctx, p, 10*time.Second); err != nil {
		return err
	}
	if p.Ready.Media != t.relayMedia || p.Ready.Leg != t.relayLeg {
		return errors.New("relay bound different socket addresses")
	}
	t.relay = p
	return nil
}

func (t *pairTopology) startStandby(ctx context.Context, r *pairRelease) error {
	t.relays++
	p, err := t.launch("relay-"+strconv.Itoa(t.relays), "relay", r, withoutKey(t.env), t.relayArgs(r, true)...)
	if err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Child.WaitLog(wait, "STANDBY_WAIT"); err != nil {
		return fmt.Errorf("%s standby wait: %w", p.Name, err)
	}
	t.standby = p
	return nil
}

func (t *pairTopology) startWorker(ctx context.Context, r *pairRelease, i int) (*pairProc, error) {
	p, err := t.launch(pairWorkerName(i), "worker", r, t.env, t.workerArgs(r, i)...)
	if err != nil {
		return nil, err
	}
	if err := t.ready(ctx, p, 10*time.Second); err != nil {
		return nil, err
	}
	if p.Ready.HTTP != "http://"+t.workerHTTP[i] {
		return nil, fmt.Errorf("%s bound %s, not its fixed endpoint", p.Name, p.Ready.HTTP)
	}
	return p, nil
}

func (t *pairTopology) startControl(ctx context.Context, r *pairRelease) (time.Duration, error) {
	started := time.Now()
	p, err := t.launch("control", "control", r, t.env, t.controlArgs(r)...)
	if err != nil {
		return 0, err
	}
	if err := t.ready(ctx, p, 10*time.Second); err != nil {
		return 0, err
	}
	t.control = p
	// The listener line is not readiness; wait for the lease and rebuild.
	_, err = t.waitControl(ctx, 20*time.Second, "mutation readiness", func(s controlplane.Status) bool { return s.Ready })
	return time.Since(started), err
}

func (t *pairTopology) startSignal(ctx context.Context, r *pairRelease) error {
	cert := filepath.Join(t.dir, fmt.Sprintf("signal-%02d.pem", t.launches+1))
	env := append(withoutKey(t.env), "RELAIS_SIGNAL_TOKEN="+t.token)
	p, err := t.launch("signal", "signal", r, env, r.binary("relais-signal"), "-listen", t.signalAddr, "-control", "http://"+t.controlAddr, "-advertise-ips", "127.0.0.1", "-cert-out", cert)
	if err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.Child.WaitLog(wait, "https="); err != nil {
		return fmt.Errorf("signal readiness: %w", err)
	}
	t.signal = p
	return t.probeSignal(ctx, cert)
}

// probeSignal proves the authenticated HTTPS frontend reaches this control.
func (t *pairTopology) probeSignal(ctx context.Context, cert string) error {
	pem, err := os.ReadFile(cert)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return errors.New("signal certificate unreadable")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+t.signalAddr+"/status", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("signal status probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
		return fmt.Errorf("signal status probe: %s", resp.Status)
	}
	return nil
}

func (t *pairTopology) boot(ctx context.Context, r *pairRelease) error {
	if err := t.startRelay(ctx, r); err != nil {
		return err
	}
	if err := t.startStandby(ctx, r); err != nil {
		return err
	}
	for i := range t.workers {
		p, err := t.startWorker(ctx, r, i)
		if err != nil {
			return err
		}
		t.workers[i] = p
	}
	if _, err := t.startControl(ctx, r); err != nil {
		return err
	}
	if err := t.startSignal(ctx, r); err != nil {
		return err
	}
	_, err := t.waitControl(ctx, 15*time.Second, "both workers live", func(s controlplane.Status) bool {
		return pairWorkerLive(s, "0", time.Time{}) && pairWorkerLive(s, "1", time.Time{})
	})
	return err
}

func (t *pairTopology) processes() []*pairProc {
	// Standby precedes the active relay: the checker keeps inventory order
	// within a role, so a compatible successor is staged before the switch.
	out := []*pairProc{t.workers[0], t.workers[1]}
	if t.standby != nil {
		out = append(out, t.standby)
	}
	return append(out, t.relay, t.control, t.signal)
}

// unexpectedExits lists current processes, and the store, that have ended.
// It is read only between rollouts, when every listed process should run.
func (t *pairTopology) unexpectedExits(store *clusterprocess.Child) []string {
	var out []string
	for _, p := range t.processes() {
		if p == nil {
			continue
		}
		select {
		case <-p.Child.Done():
			out = append(out, fmt.Sprintf("%s (%s, %v)", p.Name, p.Release.Version, p.Child.Err()))
		default:
		}
	}
	select {
	case <-store.Done():
		out = append(out, fmt.Sprintf("redis (%v)", store.Err()))
	default:
	}
	return out
}

func (t *pairTopology) close() {
	for _, p := range t.processes() {
		if p != nil {
			p.Child.Stop()
		}
	}
}

func (t *pairTopology) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s %s", url, resp.Status, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *pairTopology) post(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s %s", url, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (t *pairTopology) controlStatus(ctx context.Context) (controlplane.Status, error) {
	var s controlplane.Status
	err := t.get(ctx, "http://"+t.controlAddr+"/status", &s)
	return s, err
}

func (t *pairTopology) relayStatus(ctx context.Context) (relay.RelayStatus, error) {
	return (&controlplane.RemoteRelay{URL: "http://" + t.relayHTTP}).Status(ctx)
}

func (t *pairTopology) waitControl(ctx context.Context, timeout time.Duration, what string, done func(controlplane.Status) bool) (controlplane.Status, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		s, err := t.controlStatus(ctx)
		if err == nil && done(s) {
			return s, nil
		}
		last = err
		if time.Now().After(deadline) {
			return s, fmt.Errorf("control did not report %s within %s (last error %v)", what, timeout, last)
		}
		if err := t.pause(ctx, 50*time.Millisecond); err != nil {
			return s, err
		}
	}
}

func pairWorker(s controlplane.Status, name string) *controlplane.WorkerStatus {
	for i := range s.Workers {
		if s.Workers[i].Name == name {
			return &s.Workers[i]
		}
	}
	return nil
}

func pairWorkerLive(s controlplane.Status, name string, after time.Time) bool {
	w := pairWorker(s, name)
	return w != nil && !w.Dead && !w.Draining && !w.Recovering && !w.LivenessUnknown && w.LastHeartbeat.After(after)
}

func pairCallsOn(s controlplane.Status, name string) int {
	n := 0
	for _, c := range s.Calls {
		if c.Owner == name {
			n++
		}
	}
	return n
}

func (t *pairTopology) observe(ctx context.Context, label string) {
	s, err := t.controlStatus(ctx)
	o := pairObservation{Label: label}
	if err != nil {
		o.Err = err.Error()
	} else {
		o.LostCount, o.CrashTakeovers, o.Calls = s.LostCount, len(crashTakeovers(s.Takeovers)), len(s.Calls)
	}
	t.observations = append(t.observations, o)
	fmt.Printf("RELEASE_PAIR_OWNERSHIP at=%q calls=%d lost=%d crash_takeovers=%d error=%q\n", label, o.Calls, o.LostCount, o.CrashTakeovers, o.Err)
}

func (t *pairTopology) Inventory(ctx context.Context) (pairPool, error) {
	pool := pairPool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy"}
	for _, p := range t.processes() {
		if p == nil {
			return pool, errors.New("inventory incomplete: a role has no process")
		}
		select {
		case <-p.Child.Done():
			return pool, fmt.Errorf("inventory incomplete: %s exited: %v", p.Name, p.Child.Err())
		default:
		}
		version, commit, err := readBinaryIdentity(ctx, p.Release.binary("relais-"+p.Role))
		if err != nil {
			return pool, err
		}
		pool.Participants = append(pool.Participants, newPairParticipant(p.Name, p.Role, version, commit, p.Release))
	}
	return pool, nil
}

func (t *pairTopology) Preflight(ctx context.Context, target *pairRelease, pool pairPool, operation string) (releasecompat.Verdict, error) {
	t.checks++
	base := filepath.Join(t.dir, fmt.Sprintf("preflight-%02d-%s-%s", t.checks, operation, target.Version))
	if err := writePairPool(base+"-pool.json", pool); err != nil {
		return releasecompat.Verdict{}, err
	}
	verdict, raw, err := runPairPreflight(ctx, target.binary("relais-compat"), target.binary("compatibility.json"), base+"-pool.json", operation)
	return verdict, errors.Join(err, os.WriteFile(base+"-verdict.json", raw, 0600))
}

func (t *pairTopology) StandbyMissing() bool { return t.standby == nil }

func (t *pairTopology) ReplenishStandby(ctx context.Context, target *pairRelease) error {
	started := time.Now()
	err := t.startStandby(ctx, target)
	t.detail = fmt.Sprintf("standby_wait_ms=%.1f", pairMillis(time.Since(started)))
	return err
}

func (t *pairTopology) Apply(ctx context.Context, step releasecompat.Step, target *pairRelease) error {
	t.detail = ""
	var err error
	switch {
	case step.Role == "worker":
		i := -1
		for n := range t.workers {
			if step.Participant == pairWorkerName(n) {
				i = n
			}
		}
		if i < 0 {
			return fmt.Errorf("unknown worker %s", step.Participant)
		}
		err = t.replaceWorker(ctx, i, target)
	case step.Role == "relay" && t.standby != nil && step.Participant == t.standby.Name:
		err = t.replaceStandby(ctx, target)
	case step.Role == "relay" && step.Participant == t.relay.Name:
		err = t.handoverRelay(ctx, target)
	case step.Role == "control":
		err = t.replaceControl(ctx, target)
	case step.Role == "signal":
		err = t.replaceSignal(ctx, target)
	default:
		return fmt.Errorf("no procedure for %s %s", step.Role, step.Participant)
	}
	if err != nil {
		return err
	}
	return t.pause(ctx, t.settle)
}

// replaceWorker drains a running worker, proves it empty, stops it, waits for
// settled death, clears the sticky mark and rejoins the same endpoint.
func (t *pairTopology) replaceWorker(ctx context.Context, i int, target *pairRelease) error {
	name, peer := strconv.Itoa(i), strconv.Itoa(1-i)
	status, err := t.controlStatus(ctx)
	if err != nil {
		return err
	}
	own := pairCallsOn(status, name)
	if !pairWorkerLive(status, peer, time.Time{}) || own+pairCallsOn(status, peer) > t.callCap {
		return fmt.Errorf("survivor %s cannot absorb %d calls (cap %d)", peer, own, t.callCap)
	}
	started := time.Now()
	var drained controlplane.DrainResult
	err = t.post(ctx, "http://"+t.controlAddr+"/workers/"+name+"/drain", &drained)
	for _, res := range drained.Moves {
		for _, c := range t.calls {
			if c.SessionID() == res.ID {
				c.RecordExternalMove(res)
			}
		}
		if res.Error != "" || res.Lost {
			err = errors.Join(err, fmt.Errorf("drain move %s lost=%t: %s", res.ID, res.Lost, res.Error))
		}
	}
	if err != nil || drained.Error != "" {
		return errors.Join(err, errors.New(drained.Error))
	}
	if len(drained.Moves) != own {
		return fmt.Errorf("drain reported %d moves for %d calls", len(drained.Moves), own)
	}
	drainedAt := time.Now()
	var local mediaworker.WorkerStatus
	if err := t.get(ctx, t.workers[i].Ready.HTTP+"/status", &local); err != nil {
		return err
	}
	if local.Sessions != 0 || !local.InventoryKnown || local.Truncated || len(local.Inventory) != 0 {
		return fmt.Errorf("drained worker not empty: sessions=%d inventory=%d known=%t", local.Sessions, len(local.Inventory), local.InventoryKnown)
	}
	status, err = t.controlStatus(ctx)
	if err != nil {
		return err
	}
	if pairCallsOn(status, name) != 0 || status.Pool.Reservations != 0 {
		return fmt.Errorf("control still owns %d calls on %s with %d reservations", pairCallsOn(status, name), name, status.Pool.Reservations)
	}
	if err := t.stop(ctx, t.workers[i], 15*time.Second); err != nil {
		return err
	}
	if err := t.workers[i].Child.Err(); err != nil {
		return fmt.Errorf("drained worker exit: %w", err)
	}
	stoppedAt := time.Now()
	if _, err := t.waitControl(ctx, 20*time.Second, "worker "+name+" dead and settled", func(s controlplane.Status) bool {
		w := pairWorker(s, name)
		return w != nil && w.Dead && !w.Recovering && w.Calls == 0 && pairCallsOn(s, name) == 0
	}); err != nil {
		return err
	}
	deadAt := time.Now()
	if err := t.post(ctx, "http://"+t.controlAddr+"/private/workers/"+name+"/replace", nil); err != nil {
		return err
	}
	launched := time.Now()
	next, err := t.startWorker(ctx, target, i)
	if err != nil {
		return err
	}
	t.workers[i] = next
	if _, err := t.waitControl(ctx, 20*time.Second, "worker "+name+" rejoined", func(s controlplane.Status) bool { return pairWorkerLive(s, name, launched) }); err != nil {
		return err
	}
	t.detail = fmt.Sprintf("moved=%d drain_ms=%.1f stop_ms=%.1f dead_ms=%.1f rejoin_ms=%.1f", own, pairMillis(drainedAt.Sub(started)), pairMillis(stoppedAt.Sub(drainedAt)), pairMillis(deadAt.Sub(stoppedAt)), pairMillis(time.Since(deadAt)))
	return nil
}

// replaceStandby swaps a waiting standby, which holds no sockets or calls.
func (t *pairTopology) replaceStandby(ctx context.Context, target *pairRelease) error {
	started := time.Now()
	if err := t.stop(ctx, t.standby, 10*time.Second); err != nil {
		return err
	}
	t.standby = nil
	if err := t.startStandby(ctx, target); err != nil {
		return err
	}
	t.detail = fmt.Sprintf("standby_swap_ms=%.1f", pairMillis(time.Since(started)))
	return nil
}

// handoverRelay stops the active relay so the staged standby fences it and
// takes over the same addresses; replenishment follows a fresh preflight.
func (t *pairTopology) handoverRelay(ctx context.Context, target *pairRelease) error {
	old, successor := t.relay, t.standby
	if successor == nil || successor.Release != target {
		return errors.New("no standby of the target release is staged")
	}
	before, err := t.relayStatus(ctx)
	if err != nil {
		return err
	}
	stopped := time.Now()
	if err := old.Child.SignalGroup(syscall.SIGTERM); err != nil {
		return err
	}
	if err := t.ready(ctx, successor, 10*time.Second); err != nil {
		return err
	}
	readyAfter := time.Since(stopped)
	if successor.Ready.Media != t.relayMedia || successor.Ready.Leg != t.relayLeg || successor.Ready.HTTP != "http://"+t.relayHTTP {
		return errors.New("standby changed relay addresses")
	}
	select {
	case <-old.Child.Done():
	case <-time.After(5 * time.Second):
		return errors.New("old relay still running after the standby took over")
	}
	var after relay.RelayStatus
	for deadline := time.Now().Add(10 * time.Second); ; {
		after, err = t.relayStatus(ctx)
		if err == nil && after.Stats.Workers == 2 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("workers did not re-register with the successor relay (last error %v)", err)
		}
		if err := t.pause(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
	registered := time.Now()
	if after.Instance == before.Instance || after.Continuity == nil || before.Continuity == nil || after.Continuity.Epoch <= before.Continuity.Epoch || after.Stats.SelfFences != 0 {
		return errors.New("successor relay did not take a newer fenced tenure")
	}
	t.relayWindows = append(t.relayWindows, pairWindow{Kind: "relay", Participant: old.Name, Start: stopped, End: registered})
	t.relay, t.standby = successor, nil
	t.detail = fmt.Sprintf("ready_ms=%.1f registered_ms=%.1f routes_restored=%d epoch=%d old_exit=%v", pairMillis(readyAfter), pairMillis(registered.Sub(stopped)), after.Stats.RoutesRestored, after.Continuity.Epoch, old.Child.Err())
	return nil
}

// replaceControl starts the successor only after the predecessor has exited.
func (t *pairTopology) replaceControl(ctx context.Context, target *pairRelease) error {
	t.observe(ctx, "before control "+t.control.Name+" "+t.control.Release.Version)
	started := time.Now()
	if err := t.stop(ctx, t.control, 20*time.Second); err != nil {
		return err
	}
	if err := t.control.Child.Err(); err != nil {
		return fmt.Errorf("control exit: %w", err)
	}
	exited := time.Since(started)
	readyAfter, err := t.startControl(ctx, target)
	if err != nil {
		return err
	}
	t.observe(ctx, "after control "+target.Version)
	t.detail = fmt.Sprintf("exit_ms=%.1f ready_ms=%.1f", pairMillis(exited), pairMillis(readyAfter))
	return nil
}

func (t *pairTopology) replaceSignal(ctx context.Context, target *pairRelease) error {
	started := time.Now()
	if err := t.stop(ctx, t.signal, 10*time.Second); err != nil {
		return err
	}
	if err := t.signal.Child.Err(); err != nil {
		return fmt.Errorf("signal exit: %w", err)
	}
	if err := t.startSignal(ctx, target); err != nil {
		return err
	}
	t.detail = fmt.Sprintf("swap_ms=%.1f", pairMillis(time.Since(started)))
	return nil
}

type pairProcessIdentity struct {
	Name, Version string
	PID           int
}

func (t *pairTopology) identities() []pairProcessIdentity {
	var out []pairProcessIdentity
	for _, p := range t.processes() {
		out = append(out, pairProcessIdentity{Name: p.Name, Version: p.Release.Version, PID: p.Child.PID()})
	}
	return out
}

func runReleasePair(ctx context.Context, manager *clusterprocess.Manager, dir string, o releasePairOptions) (runErr error) {
	if o.Calls < 1 || o.Settle < 0 || o.Warmup < time.Second {
		return errors.New("release-pair needs at least one call, a non-negative settle and a warmup of at least 1s")
	}
	a, err := openPairRelease(ctx, "A", o.A, o.ASHA256, dir)
	if err != nil {
		return err
	}
	b, err := openPairRelease(ctx, "B", o.B, o.BSHA256, dir)
	if err != nil {
		return err
	}
	if c, err := releasecompat.Compare(b.Version, a.Version); err != nil || c <= 0 {
		return fmt.Errorf("release B %s must be newer than A %s (%v)", b.Version, a.Version, err)
	}
	for _, r := range []*pairRelease{a, b} {
		fmt.Printf("RELEASE_PAIR_ARCHIVE side=%s version=%s commit=%s sha256=%s archive=%s\n", r.Label, r.Version, r.Commit, r.SHA256, r.Archive)
	}
	addr, err := clusterprocess.FreeTCP()
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(addr)
	args, removeConfig, err := redisCommand(nil, o.RedisBinary, port, dir, "")
	if err != nil {
		return err
	}
	defer removeConfig()
	store, err := manager.Start(dir, "redis", os.Environ(), args...)
	if err != nil {
		return fmt.Errorf("store launch: %w", err)
	}
	defer store.Stop()
	startup, stop := context.WithTimeout(ctx, 5*time.Second)
	err = clusterprocess.WaitTCP(startup, addr, store)
	stop()
	if err != nil {
		return fmt.Errorf("store readiness: %w", err)
	}
	if err := redisendpoint.ValidateDev(addr); err != nil {
		return err
	}
	var key, token [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	env := processEnv(hex.EncodeToString(key[:]), addr, "relais:pair:"+filepath.Base(dir)+":")
	t, err := newPairTopology(manager, dir, env, hex.EncodeToString(token[:]), o.Calls, o.Settle)
	if err != nil {
		return err
	}
	defer t.close()
	booted := time.Now()
	if err := t.boot(ctx, a); err != nil {
		return fmt.Errorf("boot on %s: %w", a, err)
	}
	fmt.Printf("RELEASE_PAIR_BOOT release=%s boot_ms=%.1f relay=%s control=%s signal=%s\n", a, pairMillis(time.Since(booted)), t.relayMedia, t.controlAddr, t.signalAddr)
	relayAddr, err := netip.ParseAddrPort(t.relayMedia)
	if err != nil {
		return err
	}
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "http://" + t.controlAddr + "/calls", RelayAddr: relayAddr}})
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	for i := range o.Calls {
		call, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: i % len(t.workers), Recording: callharness.RecordingOptions{DecodeEvery: 30}})
		if err != nil {
			return fmt.Errorf("dial call %d: %w", i+1, err)
		}
		t.calls = append(t.calls, call)
	}
	// Admissions stay frozen from here on: the driver is the only caller and
	// creates no call during the rollout.
	media, stopMedia := context.WithCancel(ctx)
	defer stopMedia()
	sent := make(chan error, len(t.calls))
	for _, call := range t.calls {
		go func() { sent <- call.SendMedia(media, 30*time.Minute) }()
	}
	joined := false
	defer func() {
		stopMedia()
		if !joined {
			for range t.calls {
				<-sent
			}
		}
	}()
	if err := t.pause(ctx, o.Warmup); err != nil {
		return err
	}
	t.observe(ctx, "boot "+a.Version)
	record := func(c pairCheck) {
		fmt.Printf("RELEASE_PAIR_STEP operation=%s check=%d action=%s role=%s participant=%s preflight_ms=%.1f step_ms=%.1f %s\n", c.Operation, c.Check, c.Action, c.Role, c.Participant, pairMillis(c.Preflight), pairMillis(c.Duration), t.detail)
		t.detail = ""
	}
	if o.ExpectRefusal != "" {
		before := t.identities()
		err := pairRollout(ctx, t, b, "upgrade", record)
		var refusal *pairRefusal
		if !errors.As(err, &refusal) || refusal.Check != 1 || refusal.Refusal.Code != o.ExpectRefusal {
			return fmt.Errorf("NEGATIVE CONTROL FAILED: expected %s at the first preflight, got %v", o.ExpectRefusal, err)
		}
		after := t.identities()
		if fmt.Sprint(before) != fmt.Sprint(after) {
			return fmt.Errorf("NEGATIVE CONTROL FAILED: processes changed after refusal: %v -> %v", before, after)
		}
		if exits := t.unexpectedExits(store); len(exits) > 0 {
			return fmt.Errorf("NEGATIVE CONTROL FAILED: processes exited: %v", exits)
		}
		if _, err := t.Inventory(ctx); err != nil {
			return fmt.Errorf("NEGATIVE CONTROL FAILED: %w", err)
		}
		t.observe(ctx, "after refusal")
		if last := t.observations[len(t.observations)-1]; last.Err != "" || last.Calls != o.Calls || last.LostCount != 0 {
			return fmt.Errorf("NEGATIVE CONTROL FAILED: ownership after refusal %+v", last)
		}
		fmt.Printf("NEGATIVE_CONTROL PASS code=%s reason=%q processes_replaced=0 processes=%v\n", refusal.Refusal.Code, refusal.Refusal.Reason, after)
		return nil
	}
	upgradeStart := time.Now()
	if err := pairRollout(ctx, t, b, "upgrade", record); err != nil {
		return err
	}
	rollbackStart := time.Now()
	fmt.Printf("RELEASE_PAIR_PHASE operation=upgrade target=%s ms=%.1f\n", b, pairMillis(rollbackStart.Sub(upgradeStart)))
	if err := pairRollout(ctx, t, a, "rollback", record); err != nil {
		return err
	}
	fmt.Printf("RELEASE_PAIR_PHASE operation=rollback target=%s ms=%.1f\n", a, pairMillis(time.Since(rollbackStart)))
	// The last worker rejoin settle is the consent and stability observation.
	if err := t.pause(ctx, max(o.Settle, callharness.FreshnessPolicy{}.EvaluationWindow())); err != nil {
		return err
	}
	t.observe(ctx, "final "+a.Version)
	until := time.Now()
	exits := t.unexpectedExits(store)
	phases := []pairWindow{{Kind: "upgrade", Start: upgradeStart, End: rollbackStart}, {Kind: "rollback", Start: rollbackStart, End: until}}
	stopMedia()
	var mediaErr error
	for range t.calls {
		if err := <-sent; err != nil && !errors.Is(err, context.Canceled) {
			mediaErr = errors.Join(mediaErr, err)
		}
	}
	joined = true
	if mediaErr != nil {
		return fmt.Errorf("caller media ended early: %w", mediaErr)
	}
	var reports []*callharness.Report
	for i, call := range t.calls {
		report, err := call.Hangup(ctx)
		if err != nil {
			return fmt.Errorf("hang up call %d: %w", i+1, err)
		}
		reports = append(reports, report)
	}
	if encoded, err := json.Marshal(reports); err != nil {
		return err
	} else if err := os.WriteFile(filepath.Join(dir, "caller-reports.json"), encoded, 0600); err != nil {
		return err
	}
	verdict := judgeReleasePair(pairEvidence{Reports: reports, ExpectedCalls: o.Calls, RelayMedia: t.relayMedia, RelayWindows: t.relayWindows, Phases: phases, ObservedUntil: until, Observations: t.observations, UnexpectedExits: exits})
	fmt.Println(verdict.Summary())
	encoded, err := json.MarshalIndent(struct {
		A, B         *pairRelease
		Verdict      pairJudgement
		RelayWindows []pairWindow
		Ownership    []pairObservation
	}{a, b, verdict, t.relayWindows, t.observations}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "release-pair-verdict.json"), encoded, 0600); err != nil {
		return err
	}
	if verdict.Outcome != "pass" {
		return fmt.Errorf("release pair %s -> %s -> %s: %s", a.Version, b.Version, a.Version, verdict.Outcome)
	}
	fmt.Printf("RELEASE_PAIR PASS %s -> %s -> %s calls=%d\n", a.Version, b.Version, a.Version, o.Calls)
	return nil
}
