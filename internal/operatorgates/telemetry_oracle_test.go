package operatorgates

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestTelemetryOracle(t *testing.T) {
	corpus, err := loadProcedureCorpus(t)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name := range corpus {
		if !procedureFixtureSelected(name) {
			continue
		}
		if len(name) != 21 {
			continue
		}
		f, err := loadProcedureFixture(corpus, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, root := range procedureRoots(f, "measure.summarizeObservations") {
			var input []struct {
				At    int64
				Hosts map[string]HostTelemetry
			}
			if err := json.Unmarshal(root.req.Inputs[0], &input); err != nil {
				t.Fatal(err)
			}
			rows := []MeasurementObservation{}
			for _, r := range input {
				rows = append(rows, MeasurementObservation{At: time.UnixMilli(r.At), Hosts: r.Hosts})
			}
			got := SummarizeObservations(rows)
			var values map[string]json.RawMessage
			if err := json.Unmarshal(root.res.Value, &values); err != nil {
				t.Fatal(err)
			}
			for key, value := range values {
				if string(value) == `{"absent":true}` {
					delete(values, key)
				}
			}
			expected, _ := json.Marshal(values)
			var want Measurement
			if err := json.Unmarshal(expected, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("MUTATION: telemetry-oracle %s/%d got %s want %s", name, root.req.ID, oracleJSON(got), root.res.Value)
			}
			checked++
		}
	}
	if checked != 28 {
		t.Fatal("missing telemetry replay")
	}
	t.Logf("%d telemetry invocations", checked)
}

func TestTelemetryAgePresence(t *testing.T) {
	complete := `{"ages":[{"checkpoint_known":true,"snapshot_age_ms":0,"checkpoint_age_ms":0}]}`
	if _, err := DecodeHostTelemetry([]byte(complete)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"checkpoint_known", "snapshot_age_ms", "checkpoint_age_ms"} {
		var host map[string][]map[string]any
		if err := json.Unmarshal([]byte(complete), &host); err != nil {
			t.Fatal(err)
		}
		delete(host["ages"][0], key)
		raw, _ := json.Marshal(host)
		if _, err := DecodeHostTelemetry(raw); err == nil {
			t.Fatal("MUTATION: telemetry-age-presence", key)
		}
	}
}
