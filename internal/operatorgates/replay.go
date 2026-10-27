package operatorgates

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Corpus is a frozen language-neutral oracle. Loading verifies exact compressed,
// decompressed and logical bytes before resolving content-addressed references.
type Corpus map[string]json.RawMessage

func LoadCorpus(dir string) (Corpus, error) {
	packed, err := os.ReadFile(filepath.Join(dir, "corpus.json.gz"))
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(z, 64<<20+1))
	_ = z.Close()
	if err != nil || len(raw) > 64<<20 {
		return nil, errors.New("oracle-size")
	}
	var archive struct {
		SchemaVersion int         `json:"schemaVersion"`
		Files         [][2]string `json:"files"`
	}
	if json.Unmarshal(raw, &archive) != nil || archive.SchemaVersion != 1 {
		return nil, errors.New("oracle-schema")
	}
	files := Corpus{}
	previous := ""
	for _, entry := range archive.Files {
		name := entry[0]
		if name <= previous || strings.Contains(name, "..") || !strings.HasSuffix(name, ".json") || !json.Valid([]byte(entry[1])) {
			return nil, errors.New("oracle-member")
		}
		previous = name
		files[name] = json.RawMessage(entry[1])
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	all := map[string][]byte{"corpus.json.gz": packed, "corpus.json": raw}
	for k, v := range files {
		all[k] = v
	}
	lines := strings.Split(strings.TrimSuffix(string(sums), "\n"), "\n")
	if len(lines) != len(all) {
		return nil, errors.New("oracle-manifest")
	}
	for _, line := range lines {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			return nil, errors.New("oracle-manifest")
		}
		data, ok := all[parts[1]]
		sum := sha256.Sum256(data)
		if !ok || hex.EncodeToString(sum[:]) != parts[0] {
			return nil, errors.New("oracle-digest")
		}
		delete(all, parts[1])
	}
	return files, nil
}
func (c Corpus) Resolve(raw json.RawMessage) (json.RawMessage, error) {
	var expand func(json.RawMessage, int) (json.RawMessage, error)
	expand = func(r json.RawMessage, depth int) (json.RawMessage, error) {
		if depth > 64 {
			return nil, errors.New("oracle-reference-depth")
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(r, &obj) == nil && obj != nil {
			if ref, ok := obj["blob"]; ok && len(obj) == 1 {
				var hash string
				if json.Unmarshal(ref, &hash) != nil || len(hash) != 64 {
					return nil, errors.New("oracle-reference")
				}
				b, ok := c["blobs/"+hash+".json"]
				sum := sha256.Sum256(b)
				if !ok || hex.EncodeToString(sum[:]) != hash {
					return nil, errors.New("oracle-reference")
				}
				return expand(b, depth+1)
			}
			for k, v := range obj {
				e, err := expand(v, depth+1)
				if err != nil {
					return nil, err
				}
				obj[k] = e
			}
			return json.Marshal(obj)
		}
		var arr []json.RawMessage
		if json.Unmarshal(r, &arr) == nil && arr != nil {
			for i, v := range arr {
				e, err := expand(v, depth+1)
				if err != nil {
					return nil, err
				}
				arr[i] = e
			}
			return json.Marshal(arr)
		}
		return r, nil
	}
	return expand(raw, 0)
}

type OracleEvent struct {
	ID         int               `json:"id"`
	Kind       string            `json:"kind"`
	Operation  string            `json:"operation"`
	Inputs     []json.RawMessage `json:"inputs"`
	Value      json.RawMessage   `json:"value"`
	Completion string            `json:"completion"`
	Certainty  string            `json:"certainty"`
	Error      struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}
type OracleFixture struct {
	SchemaVersion      int           `json:"schemaVersion"`
	Scenario           string        `json:"scenario"`
	Events             []OracleEvent `json:"events"`
	ExpectedTestResult string        `json:"expectedTestResult"`
}

func (c Corpus) Fixture(name string) (OracleFixture, error) {
	var f OracleFixture
	raw, ok := c[name]
	if !ok {
		return f, errors.New("oracle-fixture")
	}
	expanded, err := c.Resolve(raw)
	if err != nil {
		return f, err
	}
	if err = json.Unmarshal(expanded, &f); err != nil {
		return f, err
	}
	if f.SchemaVersion != 1 || f.ExpectedTestResult != "passed" {
		return f, errors.New("oracle-fixture-schema")
	}
	return f, nil
}

// Public categories are complete fixed strings. Neither resource-shaped tokens,
// arbitrary error class names nor suffixes are publication evidence.
func ErrorCategory(name, message string) string {
	if name == "Error" {
		switch message {
		case "Preview deadline expired", "Preview admission expired", "Caller preparation failed", "Caller preparation deadline", "Caller job unconfirmed", "Invalid dispatch scope", "DNS preparation missing", "Caller role missing", "Caller campaign missing",
			"Caller preparation deadline:identity-path", "Caller preparation deadline:runner-preparing",
			"command-result-unknown", "gate-deadline", "interrupted", "caller-not-near", "precondition-unhealthy", "precondition-failed", "not-implemented", "prior-gate-stopped",
			"protocol-error", "duplicate-result", "invalid-refusal", "invalid-blob", "gate-failed", "unclassified":
			return message
		}
	}
	if name == "TypeError" || name == "ValidationError" {
		return name
	}
	return "unclassified"
}

// ReplayCore compares only the named pure core projections. A nonzero count is
// never a claim that the fixture's outer gate/caller algorithms were replayed.
func ReplayCore(f OracleFixture) (int, error) {
	pending := map[int]OracleEvent{}
	now := int64(0)
	checked := 0
	for _, e := range f.Events {
		if e.Kind == "request" {
			pending[e.ID] = e
			continue
		}
		if e.Kind != "result" {
			continue
		}
		req, ok := pending[e.ID]
		if !ok {
			return checked, errors.New("oracle-result-id")
		}
		if strings.HasSuffix(e.Operation, ".now") {
			if json.Unmarshal(e.Value, &now) != nil {
				return checked, errors.New("oracle-clock")
			}
		}
		var actual any
		switch e.Operation {
		case "run.previewGateDeadline":
			if len(req.Inputs) != 2 {
				return checked, errors.New("oracle-deadline-input")
			}
			var armed int64
			if json.Unmarshal(req.Inputs[1], &armed) != nil {
				return checked, errors.New("oracle-armed")
			}
			clock := &FakeClock{Now: unixMS(now)}
			budget, err := NewBudget(clock, fmt.Sprint(armed), fmt.Sprint(armed+SessionMaximum.Milliseconds()))
			if err != nil {
				return checked, err
			}
			end, ok := budget.GateEnd()
			if !ok {
				actual = map[string]bool{"absent": true}
			} else {
				absolute, _ := budget.Wire(end)
				var n int64
				_, _ = fmt.Sscan(absolute, &n)
				actual = n
			}
		case "run.errorCategory":
			if len(req.Inputs) != 1 {
				return checked, errors.New("oracle-error-input")
			}
			var v struct{ Name, Message string }
			if json.Unmarshal(req.Inputs[0], &v) != nil {
				return checked, errors.New("oracle-error")
			}
			actual = ErrorCategory(v.Name, v.Message)
		default:
			continue
		}
		want := new(any)
		decoder := json.NewDecoder(bytes.NewReader(e.Value))
		decoder.UseNumber()
		if decoder.Decode(want) != nil {
			return checked, errors.New("oracle-expected")
		}
		a, _ := json.Marshal(actual)
		b, _ := json.Marshal(want)
		if !bytes.Equal(a, b) {
			return checked, fmt.Errorf("oracle-mismatch: %s id %d: got %s want %s", e.Operation, e.ID, a, b)
		}
		checked++
	}
	return checked, nil
}

func unixMS(ms int64) time.Time { return time.UnixMilli(ms) }
