package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

func TestCalibrationAndGatesUseRouteBoundSharedLauncher(t *testing.T) {
	now := time.Now()
	clock := operatorgates.NewRealClock()
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates}, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)}
	session, err := operatorgates.NewSession("fixture", init, clock)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	mailbox := filepath.Join(work, "mailbox")
	if err = os.Mkdir(mailbox, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := operatorcaller.OpenDirectory(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	routes, ready, diagnostics := 0, 0, 0
	unknown := false
	var log bytes.Buffer
	runner := commandRunner(func(ctx context.Context, c Command) CommandResult {
		switch c.Label {
		case "caller-route":
			routes++
			if strings.Join(c.Args, " ") != "ip -j route get 192.0.2.10" {
				t.Fatal("route selection did not use the media destination", c)
			}
			return CommandResult{Code: code(0), Stdout: []byte(`[{"prefsrc":"192.0.2.1"}]`)}
		case "caller-service":
			ready++
			if unknown {
				if strings.Contains(string(c.Input), "caller-ready") {
					return CommandResult{Code: code(0), Stdout: []byte("caller-ready\n")}
				}
				return CommandResult{Err: context.DeadlineExceeded}
			}
			if ready == 1 {
				end, _ := ctx.Deadline()
				if time.Until(end) < 105*time.Second {
					t.Fatal("calibration cannot collect ten 10-second loader probes")
				}
			}
			if !strings.Contains(string(c.Input), "caller-ready") {
				t.Fatal("caller bypassed the shared P4 readiness script")
			}
			return CommandResult{Code: code(1), Stderr: []byte("readiness refused by fixture")}
		case "caller-diagnostics":
			diagnostics++
			for _, required := range []string{"-p Result", "-p ExecMainStatus", "-p ExecMainCode", "-p ActiveState", "-p SubState", "journalctl -u ", "-n 60"} {
				if !strings.Contains(string(c.Input), required) {
					t.Fatal("unknown omitted caller diagnostics", required)
				}
			}
			return CommandResult{Code: code(0), Stdout: []byte("ActiveState=active\nSubState=running\ncaller journal reason " + strings.Repeat("x", 64) + "\n")}
		default:
			t.Fatal("unexpected caller command", c.Label)
			return CommandResult{}
		}
	})
	r := &rehearsal{work: work, run: "run-launch", bridge: "/bridge", load: "/load", clock: clock, session: session, box: operatorcaller.NewMailbox(store), runner: runner, diagnostics: &log, process: &operatorgates.ProcessConfig{ArmedAt: now}, deployment: &Deployment{Work: work}, topology: &Topology{Hosts: map[string]Host{"relay": {IP: "192.0.2.10"}}, Secrets: map[string]string{"SignalToken": strings.Repeat("x", 64)}}, report: &Report{BinarySHA256: strings.Repeat("a", 64), CallerSHA256: strings.Repeat("b", 64)}}
	r.report.Bounds.DriftPPM = 100
	// Calibration reaches the same P4 readiness seam as the gate start below.
	if err = r.calibrate(context.Background()); err == nil || err.Error() != "caller-not-ready" {
		t.Fatal("calibration bypassed real launch readiness", err)
	}
	if err = r.startCaller(context.Background(), operator.ControlRestart, 1020, "2", time.Minute, 20*time.Minute); err == nil || err.Error() != "caller-not-ready" {
		t.Fatal("gate bypassed real launch readiness", err)
	}
	for _, id := range []string{"1", "2"} {
		raw, err := os.ReadFile(filepath.Join(work, "config-"+id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var config operatorcaller.BridgeConfig
		if err = json.Unmarshal(raw, &config); err != nil || config.Bind != "192.0.2.1:0" {
			t.Fatal("caller configuration still uses a wildcard bind", config, err)
		}
	}
	if routes != 1 || ready != 2 || diagnostics != 2 {
		t.Fatal("calibration and gate launch diverged", routes, ready, diagnostics)
	}
	unknown = true
	log.Reset()
	if err = r.calibrate(context.Background()); err == nil || !session.Latched() {
		t.Fatal("unknown launch did not fail calibration and latch", err)
	}
	if diagnostics != 3 || !strings.Contains(log.String(), "ActiveState=active") || !strings.Contains(log.String(), "caller journal reason [secret]") || strings.Contains(log.String(), strings.Repeat("x", 64)) {
		t.Fatal("calibration unknown lost the active caller journal or disclosed a secret", log.String())
	}
	rows := session.Uncertainties()
	if len(rows) != 1 || rows[0].Kind != "caller.start" || rows[0].Rule != "timeout" || rows[0].CallerID != "1" {
		t.Fatal("calibration unknown lost the launch certainty rule", rows)
	}
}

func TestCallerRouteRefusesMissingOrWildcardSource(t *testing.T) {
	for _, raw := range []string{`[]`, `[{}]`, `[{"prefsrc":"0.0.0.0"}]`, `[{"prefsrc":"::1"}]`, `not-json`} {
		r := &rehearsal{topology: &Topology{Hosts: map[string]Host{"relay": {IP: "192.0.2.10"}}}, diagnostics: &bytes.Buffer{}, runner: &fakeRunner{result: CommandResult{Code: code(0), Stdout: []byte(raw)}}}
		if err := r.newCaller(context.Background(), operator.Rollout, 60, "1", 4*time.Minute); err == nil || err.Error() != "caller-route-invalid" {
			t.Fatal("invalid route permitted wildcard caller launch", raw, err)
		}
	}
}
