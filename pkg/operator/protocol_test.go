package operator

import (
	"encoding/json"
	"testing"
)

func TestRequestJSONContract(t *testing.T) {
	r := Request{ID: "9007199254740993", Operation: HostExecute, Target: "shared", Action: "status", DeadlineUnixMS: "1792887600000", RemainingMS: 60000}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"9007199254740993","operation":"host.execute","target":"shared","action":"status","deadlineUnixMs":"1792887600000","remainingMs":60000}`
	if string(raw) != want {
		t.Fatalf("wire contract changed: %s", raw)
	}
	var back Request
	if err := json.Unmarshal(raw, &back); err != nil || back.ID != r.ID {
		t.Fatal("ID lost precision")
	}
}
func TestCertaintyVocabulary(t *testing.T) {
	for _, c := range []Certainty{Succeeded, Failed, Unknown} {
		if !c.Valid() {
			t.Fatal(c)
		}
	}
	for _, c := range []Certainty{"", "passed", "error"} {
		if c.Valid() {
			t.Fatal(c)
		}
	}
}
