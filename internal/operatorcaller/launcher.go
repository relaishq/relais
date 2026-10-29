package operatorcaller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

// Launcher is the container/private delivery seam. Identities and unit names
// must be allocated before Start. Calls never resubmit a start or stop.
type Launcher interface {
	Ready(context.Context, time.Time) operator.Certainty
	Start(context.Context, Identity, Phase, time.Time) StartResult
	Stop(context.Context, Identity, time.Time) StopResult
}
type StartState string

const (
	Started      StartState = "started"
	NotStarted   StartState = "proved-not-started"
	StartUnknown StartState = "unknown"
)

type StartResult struct {
	State     StartState
	Certainty operator.Certainty
	Unit      string
}
type StopResult struct {
	Inactive  bool
	Certainty operator.Certainty
	Unit      string
}

// Execute delivers one fixed script. A positive exit from an effect is unknown:
// it does not prove that the command made no change.
type Execution struct {
	UncertaintyRule string
	Certainty       operator.Certainty
	ExitCode        int
	Output          string
	NotSubmitted    bool
	RefusalCode     string
}
type Executor interface {
	Execute(context.Context, []byte, time.Time) Execution
}
type LocalExecutor struct{}

func (LocalExecutor) Execute(ctx context.Context, script []byte, end time.Time) Execution {
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Env = PublicEnvironment(nil)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return Execution{Certainty: operator.Unknown, UncertaintyRule: uncertaintyRule(ctx.Err())}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return Execution{Certainty: operator.Failed, ExitCode: exit.ExitCode(), Output: string(out)}
	}
	if err != nil {
		return Execution{Certainty: operator.Failed, NotSubmitted: true, RefusalCode: "executor-unavailable"}
	}
	return Execution{Certainty: operator.Succeeded, Output: string(out)}
}

// LaunchConfig selects MailboxPath or the hash-pinned AdapterPath. SpoolPath is
// optional in adapter mode. Adapter arguments and environment are not configurable.
type LaunchConfig struct {
	BridgePath, BridgeSHA256, LoadPath, LoadSHA256, ConfigPath, ConfigSHA256, MailboxPath, WorkRoot, Architecture string
	AdapterPath, AdapterSHA256, SpoolPath                                                                         string
	RuntimeDeadline                                                                                               time.Time
	CPUAffinity                                                                                                   string
}

func (c LaunchConfig) validate() error {
	if (c.MailboxPath == "") == (c.AdapterPath == "") || c.AdapterPath == "" && (c.AdapterSHA256 != "" || c.SpoolPath != "") {
		return errors.New("invalid-mailbox-mode")
	}
	paths := []string{c.BridgePath, c.LoadPath, c.ConfigPath, c.WorkRoot}
	hashes := []string{c.BridgeSHA256, c.LoadSHA256, c.ConfigSHA256}
	if c.AdapterPath != "" {
		paths = append(paths, c.AdapterPath)
		hashes = append(hashes, c.AdapterSHA256)
		if c.SpoolPath != "" {
			paths = append(paths, c.SpoolPath)
		}
	} else {
		paths = append(paths, c.MailboxPath)
	}
	for _, s := range paths {
		if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\r\n\x00") {
			return errors.New("invalid-launch-path")
		}
	}
	for _, s := range hashes {
		if !digestPattern.MatchString(s) {
			return errors.New("invalid-launch-hash")
		}
	}
	if c.CPUAffinity != "" && !regexp.MustCompile(`^[0-9]+( [0-9]+)*$`).MatchString(c.CPUAffinity) {
		return errors.New("invalid-caller-affinity")
	}
	if c.RuntimeDeadline.IsZero() {
		return errors.New("missing-runtime-deadline")
	}
	if c.Architecture != "x86_64" && c.Architecture != "aarch64" {
		return errors.New("invalid-launch-architecture")
	}
	return nil
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func ReadyScript(c LaunchConfig) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	adapterCheck := ""
	if c.AdapterPath != "" {
		adapterCheck = fmt.Sprintf("step=adapter-hash\n[ \"$(sha256sum %s | cut -d ' ' -f 1)\" = %s ]\n", quote(c.AdapterPath), quote(c.AdapterSHA256))
	}
	return []byte(fmt.Sprintf(`set -eu
step=architecture
trap 'status=$?; if [ "$status" -ne 0 ]; then printf "caller-ready-%%s-failed\n" "$step" >&2; fi' EXIT
[ "$(uname -m)" = %s ]
step=ffmpeg
command -v ffmpeg >/dev/null
step=systemd
command -v systemd-run >/dev/null
systemctl show --property=Version --value >/dev/null
step=bridge-hash
[ "$(sha256sum %s | cut -d ' ' -f 1)" = %s ]
step=load-hash
[ "$(sha256sum %s | cut -d ' ' -f 1)" = %s ]
step=config-hash
[ "$(sha256sum %s | cut -d ' ' -f 1)" = %s ]
%sstep=host
%s caller-host >/dev/null
step=clock
chronyc tracking | grep -Eq '^Leap status[[:space:]]*:[[:space:]]*Normal$'
printf 'caller-ready\n'
`, quote(c.Architecture), quote(c.BridgePath), quote(c.BridgeSHA256), quote(c.LoadPath), quote(c.LoadSHA256), quote(c.ConfigPath), quote(c.ConfigSHA256), adapterCheck, quote(c.BridgePath))), nil
}
func StartScript(c LaunchConfig, i Identity, end time.Time) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if c.RuntimeDeadline.Before(end) || time.Until(c.RuntimeDeadline) > 20*time.Minute {
		return nil, errors.New("invalid-runtime-deadline")
	}
	affinity := ""
	if c.CPUAffinity != "" {
		affinity = " --property=" + quote("CPUAffinity="+c.CPUAffinity)
	}
	identity, _ := json.Marshal(i)
	mailboxArgs := "-mailbox " + quote(c.MailboxPath)
	adapterStage := ""
	if c.AdapterPath != "" {
		adapterStage = fmt.Sprintf(`cp %s "$work/mailbox-adapter"
[ "$(sha256sum "$work/mailbox-adapter" | cut -d ' ' -f 1)" = %s ]
chmod 700 "$work/mailbox-adapter"
`, quote(c.AdapterPath), quote(c.AdapterSHA256))
		mailboxArgs = "-adapter \"$work/mailbox-adapter\" -deadline " + quote(c.RuntimeDeadline.UTC().Format(time.RFC3339Nano))
		if c.SpoolPath != "" {
			mailboxArgs += " -spool " + quote(c.SpoolPath)
		}
	}
	return []byte(fmt.Sprintf(`set -eu
umask 077
unit=%s
work=%s/"$unit"
remaining=$((%d - $(date +%%s)))
[ "$remaining" -gt 0 ]
mkdir "$work"
cp %s "$work/bridge"
cp %s "$work/relais-load"
cp %s "$work/config.json"
[ "$(sha256sum "$work/bridge" | cut -d ' ' -f 1)" = %s ]
[ "$(sha256sum "$work/relais-load" | cut -d ' ' -f 1)" = %s ]
[ "$(sha256sum "$work/config.json" | cut -d ' ' -f 1)" = %s ]
chmod 700 "$work/bridge" "$work/relais-load"
%ssystemd-run --quiet --service-type=exec --unit="$unit" --property="RuntimeMaxSec=$remaining" --property=TimeoutStopSec=180 --property=KillMode=control-group --property=UMask=0077 --property="WorkingDirectory=$work" --setenv=GOMAXPROCS=2%s "$work/bridge" caller -config "$work/config.json" %s -load "$work/relais-load" -identity %s
printf 'caller-started:%%s\n' "$unit"
`, quote(i.Unit()), quote(c.WorkRoot), c.RuntimeDeadline.Unix(), quote(c.BridgePath), quote(c.LoadPath), quote(c.ConfigPath), quote(c.BridgeSHA256), quote(c.LoadSHA256), quote(c.ConfigSHA256), adapterStage, affinity, mailboxArgs, quote(string(identity)))), nil
}
func StopScript(i Identity) ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	// A collected transient unit makes stop fail even though nothing is live.
	// Observe exact-unit proof before submission; never turn another stop error
	// or a lost reply into success because a later read happens to be inactive.
	return []byte(fmt.Sprintf(`export LC_ALL=C
unit=%s
proved=0
state=$(systemctl show --property=Id --property=ActiveState --property=SubState "$unit" 2>/dev/null)
show_status=$?
if [ "$show_status" -eq 0 ] || [ "$show_status" -eq 1 ]; then
  expected=$(printf 'Id=%%s\nActiveState=inactive\nSubState=dead\n' "$unit" | LC_ALL=C sort)
  if [ "$(printf '%%s\n' "$state" | LC_ALL=C sort)" = "$expected" ]; then proved=1; fi
fi
if [ "$proved" -eq 0 ] && [ -z "$state" ]; then
  journal=$(journalctl "UNIT=$unit" _PID=1 --no-pager -o cat -n 60 2>/dev/null)
  journal_status=$?
  if [ "$journal_status" -eq 0 ] && printf '%%s\n' "$journal" | grep -Fqx "$unit: Deactivated successfully."; then proved=1; fi
fi
output=$(systemctl stop --no-block "$unit" 2>&1)
status=$?
if [ "$status" -eq 0 ]; then exit 0; fi
if [ "$status" -eq 5 ] && [ "$proved" -eq 1 ] && [ "$output" = "Failed to stop $unit: Unit $unit not loaded." ]; then
  printf 'caller-collected:%%s\n' "$unit"
  exit 0
fi
printf '%%s\n' "$output" >&2
exit "$status"
`, quote(i.Unit()))), nil
}
func InactiveScript(i Identity) ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	return []byte("systemctl show --property=LoadState --property=ActiveState " + quote(i.Unit()) + "\n"), nil
}
func inactive(x Execution) bool {
	if x.Certainty == operator.Unknown || x.Certainty != operator.Succeeded && x.Certainty != operator.Failed {
		return false
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(x.Output), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || (k != "LoadState" && k != "ActiveState") || fields[k] != "" {
			return false
		}
		fields[k] = v
	}
	if len(fields) != 2 {
		return false
	}
	if fields["LoadState"] == "not-found" && fields["ActiveState"] == "inactive" {
		return x.Certainty == operator.Succeeded && x.ExitCode == 0 || x.Certainty == operator.Failed && x.ExitCode == 1
	}
	return x.Certainty == operator.Succeeded && x.ExitCode == 0 && fields["LoadState"] == "loaded" && (fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed")
}

// ObservedInactive shares the stop proof with independent resource cleanup.
// A collected transient unit is inactive only with its exact systemd observation.
func ObservedInactive(x Execution) bool { return inactive(x) }

// AwaitInactive bounds read-only stop confirmation for the launcher and cleanup.
// It does not repair the certainty of an earlier stop submission.
func AwaitInactive(ctx context.Context, executor Executor, i Identity, end time.Time) bool {
	read, err := InactiveScript(i)
	if err != nil {
		return false
	}
	for time.Now().Before(end) && ctx.Err() == nil {
		if inactive(executor.Execute(ctx, read, end)) {
			return true
		}
		if sleep(ctx, min(100*time.Millisecond, time.Until(end))) != nil {
			break
		}
	}
	return false
}

// Fence binds the launcher and broker to the engine's single fatal latch.
type Fence interface {
	Latched() bool
	Halt(string)
}
type FixedLauncher struct {
	configs map[Identity]LaunchConfig
	Exec    Executor
	Mailbox *Mailbox
	Fence   Fence
	mu      sync.Mutex
	starts  map[string]StartResult
	owners  map[string]Identity
	stops   map[string]StopResult
	ready   bool
}

// NewFixedLauncher copies the identity-bound configuration set before effects.
func NewFixedLauncher(configs map[Identity]LaunchConfig, executor Executor, box *Mailbox, fence Fence) (*FixedLauncher, error) {
	if len(configs) == 0 || executor == nil || box == nil || box.Store == nil || fence == nil {
		return nil, errors.New("missing-launcher-input")
	}
	l := &FixedLauncher{configs: map[Identity]LaunchConfig{}, Exec: executor, Mailbox: box, Fence: fence}
	for identity, config := range configs {
		if err := identity.Validate(); err != nil {
			return nil, err
		}
		if err := config.validate(); err != nil {
			return nil, err
		}
		l.configs[identity] = config
	}
	return l, nil
}
func (l *FixedLauncher) Ready(ctx context.Context, end time.Time) operator.Certainty {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ready = false
	identities := make([]Identity, 0, len(l.configs))
	for identity := range l.configs {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(a, b int) bool { return identities[a].Unit() < identities[b].Unit() })
	if len(identities) == 0 {
		return operator.Failed
	}
	for _, identity := range identities {
		script, err := ReadyScript(l.configs[identity])
		if err != nil {
			return operator.Failed
		}
		r := l.Exec.Execute(ctx, script, end)
		if r.Certainty == operator.Unknown {
			rule := r.UncertaintyRule
			if rule == "" {
				rule = "lost-reply"
			}
			uncertainty(l.Fence, "caller.ready", identity.CallerID, identity.CallerID, "ready", end, time.Now(), rule)
			return operator.Unknown
		}
		if r.Certainty != operator.Succeeded || r.ExitCode != 0 || r.Output != "caller-ready\n" || ctx.Err() != nil || !time.Now().Before(end) {
			return operator.Failed
		}
	}
	l.ready = true
	return operator.Succeeded
}
func (l *FixedLauncher) Start(ctx context.Context, i Identity, phase Phase, end time.Time) StartResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := StartResult{NotStarted, operator.Failed, i.Unit()}
	if i.Validate() != nil || phase != i.Phase {
		return result
	}
	key := i.RunID + "/" + string(i.Gate) + "/" + string(phase)
	if l.starts == nil {
		l.starts = map[string]StartResult{}
	}
	if cached, exists := l.starts[key]; exists {
		if l.owners[key] == i {
			return cached
		}
		if cached.Unit == i.Unit() {
			l.Fence.Halt("protocol-error")
			return StartResult{StartUnknown, operator.Unknown, i.Unit()}
		}
		return result
	}
	if !l.ready || l.Fence.Latched() || !time.Now().Before(end) {
		return result
	}
	config, exists := l.configs[i]
	if !exists {
		return result
	}
	script, err := StartScript(config, i, end)
	if err != nil {
		return result
	}
	// Reserve the identity before delivery. A lost result stays cached.
	l.starts[key] = StartResult{StartUnknown, operator.Unknown, i.Unit()}
	if l.owners == nil {
		l.owners = map[string]Identity{}
	}
	l.owners[key] = i
	r := l.Exec.Execute(ctx, script, end)
	if !r.NotSubmitted && r.Certainty == operator.Succeeded && r.ExitCode == 0 && r.Output == "caller-started:"+i.Unit()+"\n" && time.Now().Before(end) {
		result = StartResult{Started, operator.Succeeded, i.Unit()}
	} else if r.NotSubmitted && r.Certainty == operator.Failed && (r.RefusalCode == "executor-unavailable" || r.RefusalCode == "deadline-before-submission") {
		result = StartResult{NotStarted, operator.Failed, i.Unit()}
	} else {
		result = StartResult{StartUnknown, operator.Unknown, i.Unit()}
		rule := r.UncertaintyRule
		if rule == "" {
			rule = "malformed-reply"
			if r.Certainty == operator.Unknown {
				rule = "lost-reply"
			} else if r.Certainty != operator.Succeeded || r.ExitCode != 0 {
				rule = "unconfirmed-exit"
			}
		}
		if !time.Now().Before(end) {
			rule = "timeout"
		}
		uncertainty(l.Fence, "caller.start", i.CallerID, i.CallerID, "start", end, time.Now(), rule)
		l.Fence.Halt("command-result-unknown")
	}
	l.starts[key] = result
	return result
}

// NeverStarted proves no start was submitted for this configured identity, or
// that its submission was definitely refused. Unknown starts never satisfy it.
func (l *FixedLauncher) NeverStarted(i Identity) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, known := l.configs[i]; !known || i.Validate() != nil {
		return false
	}
	key := i.RunID + "/" + string(i.Gate) + "/" + string(i.Phase)
	start, attempted := l.starts[key]
	_, reserved := l.owners[key]
	return !attempted && !reserved || attempted && start.Unit == i.Unit() && start.State == NotStarted && start.Certainty == operator.Failed
}

func (l *FixedLauncher) Stop(ctx context.Context, i Identity, end time.Time) StopResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := StopResult{Certainty: operator.Unknown, Unit: i.Unit()}
	if i.Validate() != nil {
		return result
	}
	key := i.RunID + "/" + string(i.Gate) + "/" + string(i.Phase)
	if owner, ok := l.owners[key]; !ok || owner != i {
		return StopResult{Certainty: operator.Failed, Unit: i.Unit()}
	}

	if l.stops == nil {
		l.stops = map[string]StopResult{}
	}
	if cached, ok := l.stops[i.Unit()]; ok {
		return cached
	}
	l.stops[i.Unit()] = result
	// Hangup is outside numbered feed and remains allowed after the fence.
	uncertain := !Hangup(ctx, l.Mailbox, i.Root(), i.RunID, "caller-stop", l.Fence)
	script, _ := StopScript(i)
	if !l.Fence.Latched() {
		r := l.Exec.Execute(ctx, script, end)
		if r.Certainty != operator.Succeeded || r.ExitCode != 0 {
			uncertain = true
			rule := r.UncertaintyRule
			if rule == "" {
				rule = "unconfirmed-stop"
			}
			uncertainty(l.Fence, "caller.stop", i.CallerID, i.CallerID, "stop", end, time.Now(), rule)
			l.Fence.Halt("command-result-unknown")
		}
	} else {
		uncertain = true
	}
	result.Inactive = AwaitInactive(ctx, l.Exec, i, end)
	if result.Inactive && !uncertain {
		result.Certainty = operator.Succeeded
	} else {
		uncertainty(l.Fence, "caller.stop", i.CallerID, i.CallerID, "stop", end, time.Now(), "unconfirmed-stop")
		l.Fence.Halt("command-result-unknown")
	}
	l.stops[i.Unit()] = result
	return result
}
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(max(0, d))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func sequenceKey(root string, n int) string {
	if n == -1 {
		return root + "/feed/begin.json"
	}
	return root + "/feed/" + fmt.Sprintf("%05d", n) + ".json"
}
func ackKey(job string, n int) string { return job + "/acks/" + fmt.Sprintf("%05d", n) + ".json" }
func PublicEnvironment(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		switch key {
		case "PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ":
			out = append(out, e)
		}
	}
	return append(out, "GOMAXPROCS=2")
}
