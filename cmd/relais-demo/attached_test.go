package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func attachFlags(t *testing.T, args ...string) (config, error) {
	t.Helper()
	var cfg config
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configureFlags(fs, &cfg)
	err := fs.Parse(args)
	if err == nil {
		err = validateConfig(cfg)
	}
	return cfg, err
}
func TestAttachFlagContract(t *testing.T) {
	cfg, err := attachFlags(t, "-attach-control=http://localhost:12345", "-http=127.0.0.1:0", "-fault-hook=/test/hook", "-results-dir=/test/results")
	require.NoError(t, err)
	require.Equal(t, 90*time.Second, cfg.ActionTimeout)
	require.Equal(t, "/test/hook", cfg.FaultHook)
	require.Equal(t, "/test/results", cfg.ResultsDir)
	for _, args := range [][]string{
		{"-attach-control=http://localhost:12345", "-redis=127.0.0.1:12346"},
		{"-attach-control=http://localhost:12345", "-local-network=127.0.0.1"},
		{"-attach-control=http://localhost:12345", "-http=localhost:12346"},
		{"-attach-control=http://user:secret@localhost:12345"},
		{"-attach-control=http://localhost:12345/path"},
		{"-attach-control=http://localhost:12345?token=secret"},
		{"-attach-control=file:///tmp/control"},
		{"-action-timeout=0"}, {"-action-timeout=-1s"}, {"-action-timeout=bad"},
		{"-attach-auth-file=/test/auth"}, {"-fault-hook=/test/hook"},
	} {
		_, err := attachFlags(t, args...)
		require.Error(t, err, args)
	}
	cfg, err = attachFlags(t, "-action-timeout=2m")
	require.NoError(t, err)
	require.Equal(t, 2*time.Minute, cfg.ActionTimeout)
	token, err := configToken(config{AttachControl: "http://localhost:12345"})
	require.NoError(t, err)
	require.Len(t, token, 64)
	require.Equal(t, "http://localhost:12345/?source=camera&autostart=0#token="+token, demoPageURL(config{AttachControl: "http://localhost:12346"}, "127.0.0.1:12345", token))
}

func TestAttachAuthMatchesUnauthenticatedPrivateAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth")
	require.NoError(t, os.WriteFile(path, []byte("private-secret"), 0600))
	err := readAttachAuth(path)
	require.ErrorContains(t, err, "no credential authentication")
	require.NotContains(t, err.Error(), "private-secret")
	require.NoError(t, os.Chmod(path, 0640))
	require.ErrorContains(t, readAttachAuth(path), "0600")
	require.NoError(t, os.Chmod(path, 0400))
	require.ErrorContains(t, readAttachAuth(path), "no credential authentication")
	require.NoError(t, os.Chmod(path, 0600))
	require.NoError(t, os.WriteFile(path, nil, 0600))
	require.NoError(t, readAttachAuth(path))
	require.NoError(t, readAttachAuth(""))
	require.Error(t, readAttachAuth(t.TempDir()))
}
func scriptHook(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+source+"\n"), 0700))
	return path
}

const validKillReceipt = `{"ok":true,"action":"kill","worker":"a","pid":123,"killedAt":"2026-10-13T12:00:00.123456789Z"}`
const validReplaceReceipt = `{"ok":true,"action":"replace","worker":"a","replacement":"c","readyAt":"2026-10-13T12:00:01Z"}`

func TestAttachHookProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, action, stdout, extra string
		ok                          bool
	}{
		{name: "kill", action: "kill", stdout: validKillReceipt, ok: true},
		{name: "replace", action: "replace", stdout: validReplaceReceipt, ok: true},
		{name: "malformed", action: "kill", stdout: "not-json"},
		{name: "two objects", action: "kill", stdout: validKillReceipt + "\n" + validKillReceipt},
		{name: "false", action: "kill", stdout: strings.Replace(validKillReceipt, `"ok":true`, `"ok":false`, 1)},
		{name: "wrong worker", action: "kill", stdout: strings.Replace(validKillReceipt, `"worker":"a"`, `"worker":"b"`, 1)},
		{name: "wrong action", action: "kill", stdout: validReplaceReceipt},
		{name: "missing time", action: "kill", stdout: `{"ok":true,"action":"kill","worker":"a","pid":123}`},
		{name: "non UTC", action: "kill", stdout: strings.Replace(validKillReceipt, "Z", "+01:00", 1)},
		{name: "bad PID", action: "kill", stdout: strings.Replace(validKillReceipt, "123,", "0,", 1)},
		{name: "unknown field", action: "kill", stdout: strings.Replace(validKillReceipt, `"ok":true`, `"extra":1,"ok":true`, 1)},
		{name: "nonzero", action: "kill", stdout: validKillReceipt, extra: "exit 2"},
		{name: "timeout", action: "kill", extra: "exec /bin/sleep 60"},
		{name: "oversized stdout", action: "kill", extra: "head -c 70000 /dev/zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "printf '%s\\n' '" + tc.stdout + "'\n" + tc.extra
			d := &demo{faultHook: scriptHook(t, source)}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			receipt, err := d.runHook(ctx, tc.action, "a", "call")
			if tc.ok {
				require.NoError(t, err)
				require.True(t, receipt.OK)
			} else {
				require.Error(t, err)
				require.False(t, receipt.OK)
			}
			if tc.name == "timeout" {
				require.ErrorContains(t, err, "timeout")
			}
		})
	}
}
func TestAttachHookArgumentsEnvironmentAndStderrBound(t *testing.T) {
	t.Setenv("RELAIS_TEST_HOOK_SECRET", "private-hook-secret")
	source := `[ "$1" = kill ] && [ "$2" = --worker ] && [ "$3" = a ] && [ "$4" = --session ] && [ "$5" = call ] && [ "$#" = 5 ] || exit 1
[ "$RELAIS_TEST_HOOK_SECRET" = private-hook-secret ] || exit 1
printf '%s\n' '` + validKillReceipt + `'`
	d := &demo{faultHook: scriptHook(t, source)}
	_, err := d.runHook(context.Background(), "kill", "a", "call")
	require.NoError(t, err)
	d.faultHook = scriptHook(t, `head -c 12000 /dev/zero >&2
printf '\nend secret=%s\n' "$RELAIS_TEST_HOOK_SECRET" >&2
exit 1`)
	_, err = d.runHook(context.Background(), "kill", "a", "call")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-hook-secret")
	require.Contains(t, err.Error(), "[redacted]")
	tail := strings.SplitN(err.Error(), "; stderr: ", 2)
	require.Len(t, tail, 2)
	require.LessOrEqual(t, len(tail[1]), 4096)
}
func TestAttachLaunchOwnsNoProcessesAndStatusUsesControl(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		privateapi.Write(w, controlplane.Status{Workers: []controlplane.WorkerStatus{
			{Name: "a", LastHeartbeat: time.Now()}, {Name: "b", LastHeartbeat: time.Now()},
			{Name: "old", Dead: true, LastHeartbeat: time.Now()},
		}})
	}))
	defer control.Close()
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	d, err := launch(context.Background(), manager, config{AttachControl: control.URL, HTTP: "127.0.0.1:0", Bin: "missing-bin", RedisBinary: "missing-redis"})
	require.NoError(t, err)
	require.Nil(t, d.workers)
	require.Nil(t, d.spawn)
	require.Empty(t, d.redis)
	require.Empty(t, d.dir)
	status, err := d.status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, status.PoolSize)
	require.Equal(t, 2, status.ExpectedPoolSize)
	require.Empty(t, status.WorkerPIDs)
	require.True(t, status.AttachMode)
	require.NotEmpty(t, d.launchToken)
	_, err = d.kill(context.Background(), "call")
	require.ErrorContains(t, err, "-fault-hook is required")
	_, err = d.drain(context.Background(), "a")
	require.ErrorContains(t, err, "unsupported in attach mode")
}
func TestAttachTokenAndProxyGuards(t *testing.T) {
	var mutations atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutations.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"control failure","codes":["lease_lost"],"result":{"id":"call"}}`))
	}))
	defer control.Close()
	d := &demo{attached: true, ctx: context.Background(), launchToken: "token", launchID: "launch", control: control.URL}
	handler := d.handler(http.NotFoundHandler())
	for _, token := range []string{"", "wrong", "token"} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/calls/call/move", nil)
		req.Header.Set("X-Relais-Demo-Token", token)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if token != "token" {
			require.Equal(t, http.StatusForbidden, out.Code)
		} else {
			require.Equal(t, http.StatusConflict, out.Code)
			require.JSONEq(t, `{"error":"control failure","codes":["lease_lost"],"result":{"id":"call"}}`, out.Body.String())
		}
	}
	require.Equal(t, int32(1), mutations.Load())
	for _, target := range []string{"http://evil.example/demo/kill", "http://localhost/demo/kill"} {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.Header.Set("X-Relais-Demo-Token", "token")
		req.Header.Set("Origin", "http://evil.example")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		require.Equal(t, http.StatusForbidden, out.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/demo/drain", nil)
	req.Header.Set("X-Relais-Demo-Token", "token")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	require.Equal(t, http.StatusConflict, out.Code)
	var receipt actionResult
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &receipt))
	require.Equal(t, "unsupported in attach mode", receipt.Error)
}

type attachTransport func(*http.Request) (*http.Response, error)

func (f attachTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the production HTTP action handler without sockets. Control changes
// ownership early, then publishes the terminal event. Registration first lacks
// a heartbeat; a ready hook reply alone must not complete the action.
func TestAttachKillReceiptWaitsForTakeoverAndHealthyPool(t *testing.T) {
	dir := t.TempDir()
	killed := filepath.Join(dir, "killed")
	replaced := filepath.Join(dir, "replaced")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	hook := scriptHook(t, `if [ "$1" = kill ]; then
: > `+quote(killed)+`
printf '%s\n' '`+validKillReceipt+`'
else
[ -f `+quote(filepath.Join(dir, "completed"))+` ] || exit 1
: > `+quote(replaced)+`
printf '%s\n' '`+validReplaceReceipt+`'
fi`)
	var recoveryPolls, poolPolls int
	timestamp, err := time.Parse(time.RFC3339Nano, "2026-10-13T12:00:00.123456789Z")
	require.NoError(t, err)
	client := &http.Client{Transport: attachTransport(func(r *http.Request) (*http.Response, error) {
		status := controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: "a"}}, Workers: []controlplane.WorkerStatus{{Name: "a", LastHeartbeat: timestamp}, {Name: "b", LastHeartbeat: timestamp}}}
		if _, err := os.Stat(killed); err == nil {
			recoveryPolls++
			status.Calls[0].Owner = "b"
			status.Workers[0].Dead = true
			status.Workers[0].Recovering = recoveryPolls < 3
			// An earlier takeover for the same session/worker is never enough.
			status.Takeovers = []controlplane.MoveResult{{ID: "call", From: "a", To: "b", Start: timestamp.Add(-time.Hour), End: timestamp.Add(-time.Hour)}}
			if recoveryPolls >= 3 {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "completed"), nil, 0600))
				status.Takeovers = append(status.Takeovers, controlplane.MoveResult{ID: "call", From: "a", To: "b", Start: timestamp, End: timestamp.Add(time.Second)})
			}
		}
		if _, err := os.Stat(replaced); err == nil {
			poolPolls++
			replacement := controlplane.WorkerStatus{Name: "c"}
			if poolPolls >= 3 {
				replacement.LastHeartbeat = timestamp
			}
			status.Workers = append(status.Workers, replacement)
		}
		body, err := json.Marshal(status)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	d := &demo{ctx: context.Background(), attached: true, faultHook: hook, actionTimeout: time.Second, expectedPoolSize: 2, control: "http://control", client: client, launchToken: "token"}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/demo/kill", strings.NewReader(`{"id":"call"}`))
	req.Header.Set("X-Relais-Demo-Token", "token")
	out := httptest.NewRecorder()
	d.handler(http.NotFoundHandler()).ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	var receipt actionResult
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &receipt))
	require.Equal(t, "a", receipt.From)
	require.Equal(t, "b", receipt.To)
	require.Equal(t, "c", receipt.Replacement)
	require.Equal(t, 123, receipt.PID)
	require.True(t, timestamp.Equal(receipt.At))
	require.GreaterOrEqual(t, recoveryPolls, 3)
	require.GreaterOrEqual(t, poolPolls, 3)
}
func TestAttachHookFailuresBecomeFailedHTTPReceipts(t *testing.T) {
	for _, source := range []string{`printf 'malformed'`, `printf '%s\n' '{"ok":false}'`, `echo 'password=secret-value' >&2; exit 2`, `exec /bin/sleep 60`} {
		client := &http.Client{Transport: attachTransport(func(r *http.Request) (*http.Response, error) {
			body := `{"calls":[{"id":"call","owner":"a"}],"workers":[{"name":"a","last_heartbeat":"2026-10-13T12:00:00Z"},{"name":"b","last_heartbeat":"2026-10-13T12:00:00Z"}]}`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		d := &demo{ctx: context.Background(), attached: true, faultHook: scriptHook(t, source), actionTimeout: 100 * time.Millisecond, expectedPoolSize: 2, control: "http://control", client: client, launchToken: "token"}
		req := httptest.NewRequest(http.MethodPost, "http://localhost/demo/kill", strings.NewReader(`{"id":"call"}`))
		req.Header.Set("X-Relais-Demo-Token", "token")
		out := httptest.NewRecorder()
		d.handler(http.NotFoundHandler()).ServeHTTP(out, req)
		require.Equal(t, http.StatusConflict, out.Code)
		var receipt actionResult
		require.NoError(t, json.Unmarshal(out.Body.Bytes(), &receipt))
		require.Equal(t, "kill", receipt.Kind)
		require.Equal(t, "call", receipt.ID)
		require.Equal(t, "a", receipt.From)
		require.NotEmpty(t, receipt.Error)
		require.NotContains(t, receipt.Error, "secret-value")
	}
}

func TestLocalKillStillOwnsAndRecyclesProcesses(t *testing.T) {
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	dir := t.TempDir()
	source := testProcess(t, manager, dir, "source")
	survivor := testProcess(t, manager, dir, "survivor")
	client := &http.Client{Transport: attachTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.URL.Path == "/demo/register" {
			body = []byte(`{"ok":true}`)
		} else {
			status := controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: "source"}}, Workers: []controlplane.WorkerStatus{{Name: "source"}, {Name: "survivor"}}}
			select {
			case <-source.child.Done():
				status.Calls[0].Owner = "survivor"
				status.Workers[0].Dead = true
				status.Takeovers = []controlplane.MoveResult{{ID: "call", From: "source", To: "survivor", Start: time.Now(), End: time.Now()}}
			default:
			}
			var err error
			body, err = json.Marshal(status)
			if err != nil {
				return nil, err
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	d := &demo{ctx: context.Background(), control: "http://control", client: client, workers: map[string]*workerProcess{"source": source, "survivor": survivor}, next: 3}
	d.spawn = func(_ context.Context, name string) (*workerProcess, error) {
		return testProcess(t, manager, dir, name), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	receipt, err := d.kill(ctx, "call")
	require.NoError(t, err)
	require.Equal(t, source.child.PID(), receipt.PID)
	require.Equal(t, "survivor", receipt.To)
	require.Equal(t, "3", receipt.Replacement)
	status, err := d.status(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, status.ExpectedPoolSize)
	require.Equal(t, map[string]int{"survivor": survivor.child.PID(), "3": receipt.ReplacementPID}, status.WorkerPIDs)
	select {
	case <-survivor.child.Done():
		t.Fatal("local survivor was killed")
	default:
	}
}
