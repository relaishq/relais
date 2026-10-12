package main

import (
	"context"
	"encoding/json"
	"github.com/relais/internal/loadgen"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestUnevenCallDistribution(t *testing.T) {
	if got := counts(11, 3); !reflect.DeepEqual(got, []int{4, 4, 3}) {
		t.Fatal(got)
	}
}
func TestScheduleValidation(t *testing.T) {
	base := config{Callers: 10, Processes: 2, Workers: 3, GOMAXPROCS: 1, Duration: 60 * time.Second, SampleEvery: time.Second, Limits: loadgen.DefaultLimits()}
	var events eventFlags
	if err := events.Set("10s,move,2,1"); err != nil {
		t.Fatal(err)
	}
	if err := events.Set("30s,kill,0,0"); err != nil {
		t.Fatal(err)
	}
	base.Events = events
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for _, e := range []loadgen.Event{{After: time.Second, Kind: "move", Worker: 1}, {After: 58 * time.Second, Kind: "kill", Worker: 0}, {After: 30 * time.Second, Kind: "kill", Count: 1, Worker: 0}, {After: 30 * time.Second, Kind: "restart", Worker: 0}} {
		c := base
		c.Events = []loadgen.Event{e}
		if c.validate() == nil {
			t.Fatalf("accepted %+v", e)
		}
	}
	base.Events = append(base.Events, loadgen.Event{After: 40 * time.Second, Kind: "move", Worker: 0})
	if base.validate() == nil {
		t.Fatal("accepted move to dead worker")
	}
}
func TestEnvironmentDoesNotLeakStoreKeyToCallersOrRelay(t *testing.T) {
	t.Setenv("RELAIS_SESSIONSTORE_KEY", "secret")
	t.Setenv("RELAIS_PRIVATE_NETS", "untrusted")
	t.Setenv("RELAIS_REDIS_PASSWORD", "inherited")
	env := serviceEnv("", "", "")
	for _, entry := range env {
		if entry == "RELAIS_SESSIONSTORE_KEY=secret" || entry == "RELAIS_PRIVATE_NETS=untrusted" || entry == "RELAIS_REDIS_PASSWORD=inherited" {
			t.Fatal(entry)
		}
	}
}

func TestMoveScheduleUsesEligibleCallsAndReturnsOperatorFailure(t *testing.T) {
	var moved []string
	caller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req loadgen.MoveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		moved = append(moved, req.Sessions...)
	}))
	defer caller.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"calls": []map[string]string{{"id": "a", "owner": "0"}, {"id": "b", "owner": "1"}, {"id": "c", "owner": "0"}}})
	}))
	defer control.Close()
	topology := &topology{External: externalTopology(control.URL+"/calls", netip.MustParseAddrPort("127.0.0.1:20000"))}
	result := runEvent(context.Background(), topology, []generator{{url: caller.URL, sessions: []string{"a", "b", "c"}}}, loadgen.Event{Kind: "move", Count: 2, Worker: 1})
	if result.Error != "" || result.Affected != 2 || !reflect.DeepEqual(moved, []string{"a", "c"}) {
		t.Fatalf("%+v %v", result, moved)
	}
	result = runEvent(context.Background(), topology, []generator{{url: caller.URL, sessions: []string{"a", "b", "c"}}}, loadgen.Event{Kind: "move", Count: 3, Worker: 1})
	if result.Error == "" {
		t.Fatal("insufficient sample accepted")
	}
}

func TestMoveExcludesUndetectedKilledOwners(t *testing.T) {
	var moved []string
	caller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req loadgen.MoveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		moved = append(moved, req.Sessions...)
	}))
	defer caller.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"calls": []map[string]string{{"id": "stale", "owner": "0"}, {"id": "live", "owner": "1"}, {"id": "target", "owner": "2"}}})
	}))
	defer control.Close()
	topology := &topology{External: externalTopology(control.URL+"/calls", netip.MustParseAddrPort("127.0.0.1:20000")), Killed: map[int]bool{0: true}}
	event := loadgen.Event{Kind: "move", Worker: 2}
	result := runEvent(context.Background(), topology, []generator{{url: caller.URL, sessions: []string{"stale", "live", "target"}}}, event)
	if result.Error != "" || result.Affected != 1 || !reflect.DeepEqual(result.Sessions, []string{"live"}) || !reflect.DeepEqual(moved, result.Sessions) || result.FinishedAt.Before(result.At) {
		t.Fatalf("%+v moved=%v", result, moved)
	}
}

func TestLocalCapacityAndSoakRefuseDistributedEvidence(t *testing.T) {
	r := loadgen.RunReport{Outcome: "valid", Processes: []loadgen.ProcessReport{{Calls: []loadgen.Message{{Call: &callharness.Report{Distributed: &callharness.DistributedRecording{RunID: "run"}}}}}}}
	require.Equal(t, "inconclusive", judgeCapacity("move", r, 1, 1).Verdict)
	require.Contains(t, judgeCapacity("move", r, 1, 1).Causes[0], "distributed point estimates")
	require.Equal(t, "inconclusive", judgeSoak(config{}, r).Outcome)
}

func TestDistributedRequiresExplicitDrift(t *testing.T) {
	old := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(old)
	err := runArgs([]string{"-external-caller", "-distributed", "-run-id=run", "-frontend-control-bound=1ms", "-frontend-worker-bound=1ms", "-signaling=http://127.0.0.1:9/calls", "-relay=127.0.0.1:9", "-output=" + filepath.Join(t.TempDir(), "caller.jsonl")})
	require.ErrorContains(t, err, "requires -clock-drift-ppm")
}
