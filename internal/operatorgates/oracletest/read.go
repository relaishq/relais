// Package oracletest reads the frozen function-level observation oracle for tests.
package oracletest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

//go:embed observations.json.gz
var corpus []byte

//go:embed observations.sha256
var corpusSHA string

type Event struct {
	ID         int               `json:"id"`
	Operation  string            `json:"operation"`
	Inputs     []json.RawMessage `json:"inputs"`
	Completion string            `json:"completion"`
	Value      json.RawMessage   `json:"value"`
}
type Case struct {
	Source      string  `json:"source"`
	Scenario    string  `json:"scenario"`
	Request     Event   `json:"request"`
	Result      Event   `json:"result"`
	HostResults []Event `json:"hostResults"`
}

type Observation struct {
	Source  string `json:"source"`
	Decoder string `json:"decoder"`
	Result  Event  `json:"result"`
}

func Typed(t *testing.T) []Observation {
	t.Helper()
	z, e := gzip.NewReader(bytes.NewReader(corpus))
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	var body struct {
		Typed []Observation `json:"typedObservations"`
	}
	if e := json.NewDecoder(z).Decode(&body); e != nil {
		t.Fatal(e)
	}
	if len(body.Typed) == 0 {
		t.Fatal("missing typed oracle observations")
	}
	return body.Typed
}

func Cases(t *testing.T, operation string) []Case {
	t.Helper()
	if fmt.Sprintf("%x", sha256.Sum256(corpus)) != strings.TrimSpace(corpusSHA) {
		t.Fatal("frozen observation oracle digest differs")
	}
	z, e := gzip.NewReader(bytes.NewReader(corpus))
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	data, e := io.ReadAll(z)
	if e != nil {
		t.Fatal(e)
	}
	var body struct {
		Cases []Case `json:"cases"`
	}
	if e := json.Unmarshal(data, &body); e != nil {
		t.Fatal(e)
	}
	var rows []Case
	for _, c := range body.Cases {
		if c.Request.Operation == operation {
			rows = append(rows, c)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("no oracle invocations for %s", operation)
	}
	return rows
}

func JSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if e := json.Unmarshal(data, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func Marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// ProductJSON translates the oracle's documented target OS alias at the test
// boundary. Production inventory still requires the actual Linux release.
// Other OS values and all non-OS strings remain unchanged.
func ProductJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	var value any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, field := range v {
				if key == "os" && field == "target-os" {
					v[key] = "linux"
				} else {
					visit(field)
				}
			}
		case []any:
			for _, field := range v {
				visit(field)
			}
		}
	}
	visit(value)
	return Marshal(t, value)
}
