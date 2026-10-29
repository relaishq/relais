package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

func TestMailboxAdapterSubprocess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	adapter := filepath.Join(root, "adapter")
	build := exec.CommandContext(ctx, "go", "build", "-o", adapter, "./testdata/mailbox-adapter")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "create-only", true: "unknown-latches"}[unknown], func(t *testing.T) {
			tracePath := filepath.Join(root, "trace-"+strconv.FormatBool(unknown))
			fixture, _ := json.Marshal(map[string]any{"trace": tracePath, "unknown": unknown})
			if err := os.WriteFile(filepath.Join(root, "adapter-fixture.json"), fixture, 0600); err != nil {
				t.Fatal(err)
			}
			peerCtx, stop := context.WithCancel(ctx)
			defer stop()
			peer, err := operatortransport.Start(peerCtx, adapter, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { stop(); _ = peer.Close(); _ = peer.Wait() }()
			box := &operatorcaller.ProtocolMailbox{Peer: peer, Session: "caller", End: time.Now().Add(time.Minute)}
			read, err := box.Read(ctx, "scope/missing.json")
			if err != nil || read.Found {
				t.Fatal("adapter read changed", read, err)
			}
			first, second := operatorcaller.Committed, operatorcaller.Conflict
			if unknown {
				first, second = operatorcaller.CreateUnknown, operatorcaller.CreateUnknown
			}
			if box.Create(ctx, "scope/report.json", []byte("evidence")) != first || box.Create(ctx, "scope/report.json", []byte("changed")) != second {
				t.Fatal("adapter lost create-only or unknown semantics")
			}
			trace, err := os.ReadFile(tracePath)
			want := 3
			if unknown {
				want = 2
			}
			if err != nil || bytes.Count(trace, []byte("\n")) != want {
				t.Fatal("adapter request replay", string(trace), err)
			}
		})
	}
}

// The real caller requires Linux host observations. This exercises the generated
// staging script, built caller command and subprocess adapter through setup and
// a confirmed conductor stop (or an unknown create), without claiming media.
func TestCallerBinaryUsesMailboxAdapter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the production caller requires Linux host observations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	buildRoot := t.TempDir()
	bridge := filepath.Join(buildRoot, "relais-gates")
	adapter := filepath.Join(buildRoot, "adapter")
	for output, source := range map[string]string{bridge: ".", adapter: "./testdata/mailbox-adapter"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-o", output, source)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed-stop", true: "unknown-create"}[unknown], func(t *testing.T) {
			root := t.TempDir()
			now := time.Now()
			i := operatorcaller.Identity{RunID: "run-adapter", Gate: operator.Rollout, Phase: operatorcaller.Survivor, CallerID: "1", Attempt: "1", PublicSHA: strings.Repeat("a", 40)}
			manifest := operatorcaller.Manifest{Version: 1, Identity: i, ArmedAt: now.Add(-20 * time.Second).UnixMilli(), CreatedAt: now.Add(-20 * time.Second).UnixMilli(), Deadline: now.Add(5 * time.Minute).UnixMilli(), Plan: operatorcaller.NewPlan(i.Phase, 1, i.PublicSHA), Endpoints: operatorcaller.Endpoints{RunID: i.RunID, Signaling: "https://127.0.0.1:1/calls", Relay: "192.0.2.1:24780", FrontendControlBoundNS: 1000000, FrontendWorkerBoundNS: 1000000, DriftPPM: 10}}
			if err := manifest.Validate(i, now); err != nil {
				t.Fatal(err)
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
			certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			write := func(path string, data []byte, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			hash := func(path string) string {
				t.Helper()
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(raw)
				return hex.EncodeToString(sum[:])
			}
			write(root+"/ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600)
			write(root+"/load", []byte("#!/bin/sh\nexit 99\n"), 0700)
			write(root+"/ffmpeg", []byte("#!/bin/sh\nexit 99\n"), 0700)
			config, _ := json.Marshal(operatorcaller.BridgeConfig{Identity: i, Bind: "192.0.2.1:0", CertificateFile: root + "/ca.pem", LoadSHA256: hash(root + "/load")})
			write(root+"/config.json", config, 0600)
			fixture, _ := json.Marshal(map[string]any{"manifest": manifest, "trace": root + "/trace.jsonl", "unknown": unknown})
			write(root+"/adapter-fixture.json", fixture, 0600)
			// Replace only systemd delivery: preserve the generated argv and run
			// the real staged bridge. The adapter fixture is local test input.
			stub := "#!/bin/sh\nset -eu\nwhile [ \"${1#--}\" != \"$1\" ]; do shift; done\ncp '" + root + "/adapter-fixture.json' \"$(dirname \"$1\")/adapter-fixture.json\"\nexec \"$@\"\n"
			write(root+"/systemd-run", []byte(stub), 0700)
			c := operatorcaller.LaunchConfig{BridgePath: bridge, BridgeSHA256: hash(bridge), LoadPath: root + "/load", LoadSHA256: hash(root + "/load"), ConfigPath: root + "/config.json", ConfigSHA256: hash(root + "/config.json"), AdapterPath: adapter, AdapterSHA256: hash(adapter), SpoolPath: root, WorkRoot: root, RuntimeDeadline: now.Add(9 * time.Second), Architecture: map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]}
			script, err := operatorcaller.StartScript(c, i, now.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, "/bin/sh", "-s")
			cmd.Env = append(operatorcaller.PublicEnvironment(nil), "PATH="+root+":/usr/bin:/bin", "UNRELATED_PASSWORD=must-not-reach-adapter")
			cmd.Stdin = bytes.NewReader(script)
			out, err := cmd.CombinedOutput()
			if err == nil || bytes.Contains(out, []byte("caller-started:")) {
				t.Fatal("caller did not stop at the expected boundary", string(out), err)
			}
			failure := "conductor-stopped"
			if unknown {
				failure = "host-publication-unknown"
			}
			if !bytes.Contains(out, []byte("caller-incomplete: "+failure)) {
				t.Fatal("caller did not consume the adapter outcome", string(out))
			}
			trace, err := os.ReadFile(root + "/trace.jsonl")
			if err != nil {
				t.Fatal("adapter did not serve the caller", string(out), err)
			}
			var operations []operator.Operation
			for _, line := range bytes.Split(bytes.TrimSpace(trace), []byte("\n")) {
				var frame operator.Frame
				if err := json.Unmarshal(line, &frame); err != nil || frame.Request == nil {
					t.Fatal("invalid adapter trace", err)
				}
				operations = append(operations, frame.Request.Operation)
				deadline, parseErr := strconv.ParseInt(frame.Request.DeadlineUnixMS, 10, 64)
				if parseErr != nil || deadline > c.RuntimeDeadline.UnixMilli() {
					t.Fatal("mailbox request exceeds launcher deadline", frame.Request)
				}
			}
			want := []operator.Operation{operator.MailboxRead, operator.MailboxRead, operator.MailboxCreate}
			if !unknown {
				want = append(want, operator.MailboxCreate, operator.MailboxRead)
			}
			gotJSON, _ := json.Marshal(operations)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatal("caller mailbox sequence changed or unknown was retried", string(gotJSON), string(out))
			}
		})
	}
}

func TestCallerRejectsInvalidRuntimeDeadline(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(operatorcaller.BridgeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args := os.Args
	defer func() { os.Args = args }()
	for _, deadline := range []string{"invalid", time.Now().Add(-time.Minute).Format(time.RFC3339Nano)} {
		os.Args = []string{"relais-gates", "caller", "-config", config, "-adapter", "/unused", "-deadline", deadline}
		if got := run(); got != 2 {
			t.Fatal("invalid caller runtime deadline was accepted", deadline, got)
		}
	}
}
