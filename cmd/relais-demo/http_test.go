package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func testProcess(t *testing.T, manager *clusterprocess.Manager, dir, name string) *workerProcess {
	t.Helper()
	child, err := manager.Start(dir, name, os.Environ(), "/bin/sleep", "60")
	require.NoError(t, err)
	return &workerProcess{child: child, URL: "http://127.0.0.1:12345"}
}
func TestDemoKillSelectsOwnerAndStartsReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	dir := t.TempDir()
	a := testProcess(t, manager, dir, "a")
	b := testProcess(t, manager, dir, "b")
	registered := false
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			owner, dead := "b", false
			select {
			case <-b.child.Done():
				owner, dead = "a", true
			default:
			}
			status := controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: owner}}, Workers: []controlplane.WorkerStatus{{Name: "b", Dead: dead}}}
			if dead {
				status.Takeovers = []controlplane.MoveResult{{Kind: "takeover", ID: "call", From: "b", To: "a", Start: time.Now(), End: time.Now()}}
			}
			privateapi.Write(w, status)
		case "/demo/register":
			var req map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, "3", req["name"])
			registered = true
			privateapi.Write(w, map[string]any{"ok": true})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	var replacement *workerProcess
	d := &demo{ctx: ctx, control: control.URL, client: control.Client(), workers: map[string]*workerProcess{"a": a, "b": b}, next: 3}
	d.spawn = func(ctx context.Context, name string) (*workerProcess, error) {
		replacement = testProcess(t, manager, dir, name)
		return replacement, nil
	}
	server := httptest.NewServer(d.handler(http.NotFoundHandler()))
	defer server.Close()
	var result actionResult
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/demo/kill", map[string]string{"id": "call"}, &result, nil))
	require.Equal(t, "b", result.From)
	require.Equal(t, "a", result.To)
	require.Equal(t, b.child.PID(), result.PID)
	require.True(t, registered)
	require.NotNil(t, replacement)
	require.Equal(t, replacement.child.PID(), result.ReplacementPID)
	select {
	case <-a.child.Done():
		t.Fatal("non-owner was killed")
	default:
	}
	select {
	case <-b.child.Done():
	default:
		t.Fatal("owner was not reaped")
	}
	var status demoStatus
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodGet, "/demo/status", nil, &status, nil))
	require.Equal(t, "a", status.Calls[0].Owner)
	require.Equal(t, map[string]int{"a": a.child.PID(), "3": replacement.child.PID()}, status.WorkerPIDs)
}

func TestDemoKillWaitsForTerminalTakeoverOutcome(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost=%t", lost), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			manager := &clusterprocess.Manager{}
			defer manager.Stop(false)
			dir := t.TempDir()
			worker := testProcess(t, manager, dir, "source")
			polls := 0
			oldEvent := controlplane.MoveResult{Kind: "takeover", ID: "call", From: "source", To: "old", Start: time.Now().Add(-time.Minute), End: time.Now().Add(-time.Minute)}
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/status":
					status := controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: "source"}}, Takeovers: []controlplane.MoveResult{oldEvent}}
					select {
					case <-worker.child.Done():
						polls++
						status.Workers = []controlplane.WorkerStatus{{Name: "source", Dead: true, Recovering: polls == 1 || lost && polls == 2}}
						status.Calls[0].Owner = "transient"
						if polls >= 3 {
							event := controlplane.MoveResult{Kind: "takeover", ID: "call", From: "source", To: "final", Lost: lost, Start: time.Now(), End: time.Now()}
							status.Takeovers = append(status.Takeovers, event)
							status.Calls[0].Owner = event.To
							if lost {
								status.Calls = nil
							}
						}
					default:
					}
					privateapi.Write(w, status)
				case "/demo/register":
					privateapi.Write(w, map[string]bool{"ok": true})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer control.Close()
			d := &demo{ctx: ctx, control: control.URL, client: control.Client(), workers: map[string]*workerProcess{"source": worker}, next: 3}
			d.spawn = func(_ context.Context, name string) (*workerProcess, error) {
				return testProcess(t, manager, dir, name), nil
			}
			server := httptest.NewServer(d.handler(http.NotFoundHandler()))
			defer server.Close()
			response, err := server.Client().Post(server.URL+"/demo/kill", "application/json", strings.NewReader(`{"id":"call"}`))
			require.NoError(t, err)
			defer response.Body.Close()
			var result actionResult
			require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
			if lost {
				// Check the real HTTP outcome, not just a detached decision helper.
				require.Equal(t, http.StatusConflict, response.StatusCode)
				require.Equal(t, "call was lost during takeover", result.Error)
				require.Empty(t, result.To)
			} else {
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Empty(t, result.Error)
				require.Equal(t, "final", result.To)
			}
			require.Equal(t, "3", result.Replacement)
			require.Positive(t, result.ReplacementPID)
		})
	}
}

func TestDemoStatusAndProxyAndActionGuards(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			privateapi.Write(w, controlplane.Status{Calls: []controlplane.CallStatus{{ID: "one", Owner: "a"}, {ID: "two", Owner: "b"}}})
			return
		}
		if r.URL.Path == "/calls" {
			require.Equal(t, "application/sdp", r.Header.Get("Content-Type"))
			w.Header().Set("Location", "/calls/test")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("answer"))
			return
		}
		http.NotFound(w, r)
	}))
	defer control.Close()
	d := &demo{ctx: context.Background(), control: control.URL, client: control.Client(), workers: map[string]*workerProcess{}, relay: "127.0.0.1:1111", redis: "127.0.0.1:2222", launchID: "test-launch"}
	handler := d.handler(http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodGet, "http://localhost/demo/status", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var status demoStatus
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	require.Len(t, status.Calls, 2)
	require.Equal(t, d.relay, status.Relay)
	require.Equal(t, d.redis, status.Redis)
	for _, tc := range []struct {
		body, origin string
		code         int
	}{{`{}`, "", 409}, {`{"id":"missing"}`, "", 409}, {`invalid`, "", 400}, {`{"id":"one"}`, "http://evil.example", 403}} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/demo/kill", strings.NewReader(tc.body))
		req.Header.Set("Origin", tc.origin)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		require.Equal(t, tc.code, out.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/calls", strings.NewReader("offer"))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("X-Relais-Demo-Launch-ID", d.launchID)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	require.Equal(t, http.StatusCreated, out.Code)
	require.Equal(t, "/calls/test", out.Header().Get("Location"))
	require.Equal(t, "answer", out.Body.String())
}
func TestDemoDrainRecyclesOnlyDrainedWorker(t *testing.T) {
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	dir := t.TempDir()
	a := testProcess(t, manager, dir, "a")
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workers/a/drain":
			privateapi.Write(w, controlplane.DrainResult{Moves: []controlplane.MoveResult{{ID: "call", From: "a", To: "b"}}})
		case "/demo/register":
			privateapi.Write(w, map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	d := &demo{ctx: context.Background(), control: control.URL, client: control.Client(), workers: map[string]*workerProcess{"a": a}, next: 3}
	d.spawn = func(ctx context.Context, name string) (*workerProcess, error) {
		return testProcess(t, manager, dir, name), nil
	}
	handler := d.handler(http.NotFoundHandler())
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest(http.MethodPost, "http://localhost/workers/a/drain", nil))
	require.Equal(t, http.StatusOK, out.Code)
	var result actionResult
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &result))
	require.Equal(t, "b", result.To)
	require.Equal(t, "3", result.Replacement)
	select {
	case <-a.child.Done():
	default:
		t.Fatal("drained worker was not stopped")
	}
	require.NotContains(t, d.workers, "a")
	require.Contains(t, d.workers, "3")
}
func TestDemoConfigurationRefusesReservedRedisAndPublicHTTP(t *testing.T) {
	// These guards run before Redis startup or any connection attempt.
	require.Error(t, validateConfig(config{HTTP: "127.0.0.1:0", Redis: "127.0.0.1:6379"}))
	require.Error(t, validateConfig(config{HTTP: "0.0.0.0:9101"}))
	require.NoError(t, validateConfig(config{HTTP: "127.0.0.1:0", Redis: "127.0.0.1:16379"}))
}
func TestProcessEnvDoesNotLeakSnapshotKeyToRelay(t *testing.T) {
	t.Setenv("RELAIS_SESSIONSTORE_KEY", "old-key")
	t.Setenv("RELAIS_REDIS_ADDR", "reserved:6379")
	env := processEnv("new-key", "127.0.0.1:16379", "demo:", false)
	require.NotContains(t, env, "RELAIS_SESSIONSTORE_KEY=old-key")
	require.NotContains(t, env, "RELAIS_SESSIONSTORE_KEY=new-key")
	require.Contains(t, env, "RELAIS_REDIS_ADDR=127.0.0.1:16379")
	require.Contains(t, processEnv("new-key", "127.0.0.1:16379", "demo:", true), "RELAIS_SESSIONSTORE_KEY=new-key")
}

func TestFreshUDPDoesNotReuseRegistryAddresses(t *testing.T) {
	used := map[string]bool{}
	for i := 0; i < 20; i++ {
		addr, err := freshUDP(used)
		require.NoError(t, err)
		require.Len(t, used, i+1)
		require.True(t, used[addr])
	}
}

func TestDemoHostAllowlist(t *testing.T) {
	d := &demo{}
	handler := d.handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, host := range []string{"localhost:9101", "127.0.0.1:9101", "[::1]:9101", "attacker.example:9101", "127.0.0.1.attacker.example"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
		req.Host = host
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if strings.Contains(host, "attacker") {
			require.Equal(t, http.StatusForbidden, out.Code)
		} else {
			require.Equal(t, http.StatusNoContent, out.Code)
		}
	}
}
func TestReplacementRetriesFailedSpawn(t *testing.T) {
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	dir := t.TempDir()
	registrations := 0
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { registrations++; w.WriteHeader(http.StatusOK) }))
	defer control.Close()
	attempts := 0
	d := &demo{client: control.Client(), control: control.URL, workers: map[string]*workerProcess{}, next: 3}
	d.spawn = func(_ context.Context, name string) (*workerProcess, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("readiness failed")
		}
		return testProcess(t, manager, dir, name), nil
	}
	name, _, err := d.replacement(context.Background(), "old")
	require.NoError(t, err)
	require.Equal(t, "4", name)
	require.Equal(t, 2, attempts)
	require.Equal(t, 1, registrations)
}

func TestFailedReadinessStopsChild(t *testing.T) {
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	child := testProcess(t, manager, t.TempDir(), "unregistered").child
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := readyChild(ctx, child)
	require.Error(t, err)
	select {
	case <-child.Done():
	default:
		t.Fatal("readiness error left unregistered child running")
	}
}

// The POST takes effect, then its connection closes without any response.
// Exercise the production replacement path, not a retry-only transport fake.
func TestReplacementLostRegistrationReply(t *testing.T) {
	for _, outcome := range []string{"registered", "absent", "unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			manager := &clusterprocess.Manager{}
			defer manager.Stop(false)
			var mu sync.Mutex
			registered := ""
			statusUnavailable := outcome == "unavailable"
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/demo/register":
					var req map[string]string
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					if outcome == "registered" {
						mu.Lock()
						registered = req["name"]
						mu.Unlock()
					}
					hijacker, ok := w.(http.Hijacker)
					require.True(t, ok)
					conn, _, err := hijacker.Hijack()
					require.NoError(t, err)
					_ = conn.Close()
				case "/status":
					mu.Lock()
					unavailable := statusUnavailable
					mu.Unlock()
					if unavailable {
						http.Error(w, "status unavailable", http.StatusServiceUnavailable)
						return
					}
					status := controlplane.Status{Workers: []controlplane.WorkerStatus{}}
					mu.Lock()
					if registered != "" {
						status.Workers = append(status.Workers, controlplane.WorkerStatus{Name: registered})
					}
					mu.Unlock()
					privateapi.Write(w, status)
				default:
					http.NotFound(w, r)
				}
			}))
			defer control.Close()
			var child *workerProcess
			attempts := 0
			d := &demo{client: control.Client(), control: control.URL, workers: map[string]*workerProcess{}, next: 3}
			d.spawn = func(_ context.Context, name string) (*workerProcess, error) {
				attempts++
				child = testProcess(t, manager, t.TempDir(), name)
				return child, nil
			}
			name, pid, err := d.replacement(context.Background(), "old")
			if outcome == "absent" {
				require.Error(t, err)
				require.Equal(t, 2, attempts)
				require.Zero(t, pid)
				select {
				case <-child.child.Done():
				default:
					t.Fatal("confirmed unregistered worker survived")
				}
				return
			}
			require.Equal(t, 1, attempts, "lost reply must not spawn a second worker")
			require.Equal(t, child.child.PID(), pid)
			require.Same(t, child, d.workers[name])
			select {
			case <-child.child.Done():
				t.Fatal("registered or uncertain worker was stopped")
			default:
			}
			if outcome == "registered" {
				require.NoError(t, err)
				status, statusErr := d.status(context.Background())
				require.NoError(t, statusErr)
				require.Contains(t, status.WorkerPIDs, name)
				require.Empty(t, status.RegistrationError)
			} else {
				require.ErrorIs(t, err, errRegistrationUncertain)
				require.NotEmpty(t, d.registrationError)
				mu.Lock()
				statusUnavailable = false
				registered = name
				mu.Unlock()
				status, statusErr := d.status(context.Background())
				require.NoError(t, statusErr)
				require.Empty(t, status.RegistrationError, "later confirmation must clear uncertainty")
				require.Contains(t, status.WorkerPIDs, name)
				require.Equal(t, 1, attempts)
			}
		})
	}
}

func TestDemoAgentMovePreservesFailureResult(t *testing.T) {
	result := controlplane.MoveResult{ID: "call", Kind: "move", From: "0", To: "1", Lost: true, LossCause: "ownership_expired", Error: "move failed"}
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			privateapi.Write(w, controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: "0"}}})
		case "/calls/call/move":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			privateapi.Write(w, controlplane.MoveFailure{Error: "move failed", Codes: privateapi.ErrorCodes(privateapi.Uncertain(context.DeadlineExceeded), controlplane.RemoteErrors), Result: result})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	d := &demo{ctx: context.Background(), control: control.URL, client: control.Client(), agentName: "demo"}
	server := httptest.NewServer(d.handler(http.NotFoundHandler()))
	defer server.Close()
	var returned controlplane.MoveResult
	err := privateapi.DoResult(context.Background(), server.Client(), server.URL, http.MethodPost, "/calls/call/move", nil, &returned, controlplane.RemoteErrors)
	require.ErrorIs(t, err, privateapi.ErrUncertain)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var failure *privateapi.HTTPError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusInternalServerError, failure.StatusCode)
	require.Equal(t, result, returned)
}

func TestDemoIgnoresLocal6379OptIn(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "1")
	for _, addr := range []string{"127.0.0.1:6379", "cache.example.invalid:6379"} {
		require.ErrorContains(t, validateConfig(config{HTTP: "127.0.0.1:0", Redis: addr}), "reserved")
	}
}

func TestProcessEnvDropsInheritedRedisSettings(t *testing.T) {
	for _, name := range []string{
		"RELAIS_REDIS_ADDR", "RELAIS_REDIS_PREFIX", "RELAIS_REDIS_PASSWORD",
		"RELAIS_REDIS_USERNAME", "RELAIS_REDIS_TLS", "RELAIS_REDIS_TLS_CA_FILE",
		"RELAIS_REDIS_TLS_SERVER_NAME", "RELAIS_REDIS_TLS_CERT_FILE", "RELAIS_REDIS_TLS_KEY_FILE",
		"RELAIS_REDIS_ALLOW_LOCAL_6379", "RELAIS_REDIS_FUTURE_SETTING",
	} {
		t.Setenv(name, "inherited-value")
	}
	t.Setenv("RELAIS_TEST_CHILD_MARKER", "preserved")
	env := processEnv("owned-key", "127.0.0.1:16379", "owned-prefix:", true)
	redisSettings := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "RELAIS_REDIS_") {
			require.NotContains(t, redisSettings, name, "owned settings must appear once")
			redisSettings[name] = value
		}
	}
	require.Equal(t, map[string]string{"RELAIS_REDIS_ADDR": "127.0.0.1:16379", "RELAIS_REDIS_PREFIX": "owned-prefix:"}, redisSettings)
	require.Contains(t, env, "RELAIS_TEST_CHILD_MARKER=preserved")
}
