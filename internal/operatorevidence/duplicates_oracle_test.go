package operatorevidence_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
)

func TestDuplicatePacketsOracle(t *testing.T) {
	corpus, err := operatorgates.LoadCorpus("../operatorgates/testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if strings.Contains(name, "/") {
			continue
		}
		f, err := corpus.Fixture(name)
		if err != nil {
			t.Fatal(err)
		}
		results := map[int]operatorgates.OracleEvent{}
		for _, e := range f.Events {
			if e.Kind == "result" {
				results[e.ID] = e
			}
		}
		for _, e := range f.Events {
			if e.Kind != "request" || e.Operation != "processes.joinedDuplicatePackets" {
				continue
			}
			var raw string
			var sessions []string
			var pid int
			_ = json.Unmarshal(e.Inputs[0], &raw)
			_ = json.Unmarshal(e.Inputs[1], &sessions)
			_ = json.Unmarshal(e.Inputs[2], &pid)
			got, err := operatorevidence.JoinedDuplicatePackets(raw, sessions, pid)
			var want *uint64
			if string(results[e.ID].Value) != `{"absent":true}` {
				if err := json.Unmarshal(results[e.ID].Value, &want); err != nil {
					t.Fatal(err)
				}
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("MUTATION: duplicate-oracle invocation %d got %v (%v) want %v", e.ID, got, err, want)
			}
			checked++
		}
	}
	if checked != 29 {
		t.Fatal("missing duplicate invocations")
	}
}
