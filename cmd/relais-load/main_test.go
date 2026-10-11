package main

import (
	"context"
	"encoding/json"
	"github.com/relais/internal/loadgen"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
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
	env := serviceEnv("", "", "")
	for _, entry := range env {
		if entry == "RELAIS_SESSIONSTORE_KEY=secret" || entry == "RELAIS_PRIVATE_NETS=untrusted" {
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
