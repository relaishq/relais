package operatorcaller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/callharness"
)

// This replays the broker algorithm above the frozen wait/write callback seam.
// Callback-level fixtures do not establish raw mailbox parsing or whole-session
// equivalence; those scopes stay pending in the inventory.
func TestOracleBrokerCallbacks(t *testing.T) {
	corpus, err := operatorgates.LoadCorpus("../operatorgates/testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"f45d2aaf8872ceb1.json", "0f0eb2dc3f770eb9.json", "33f555d1bf7e8653.json"} {
		t.Run(name, func(t *testing.T) {
			fixture, err := corpus.Fixture(name)
			if err != nil {
				t.Fatal(err)
			}
			results := map[int]operatorgates.OracleEvent{}
			requests := map[int]operatorgates.OracleEvent{}
			for _, e := range fixture.Events {
				switch e.Kind {
				case "result":
					results[e.ID] = e
				case "request":
					requests[e.ID] = e
				}
			}
			var m operatorcaller.Manifest
			for _, e := range fixture.Events {
				if e.Kind == "request" && e.Operation == "broker.validatePreviewManifest" {
					if err = json.Unmarshal(e.Inputs[0], &m); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			clock := &testClock{time.UnixMilli(m.CreatedAt)}
			box := &memoryBox{data: map[string][]byte{}}
			fence := &testFence{}
			b, err := operatorcaller.NewBroker(m, operatorcaller.NewMailbox(box), clock, fence)
			if err != nil {
				t.Fatal(err)
			}
			type publication struct {
				key     string
				value   json.RawMessage
				outcome operatorcaller.CreateState
			}
			var expected []publication
			for _, e := range fixture.Events {
				if e.Kind != "request" {
					continue
				}
				if e.Operation == "broker.PreviewBroker.write" {
					var key string
					_ = json.Unmarshal(e.Inputs[0], &key)
					r := results[e.ID]
					state := operatorcaller.Committed
					if r.Completion == "threw" {
						state = operatorcaller.CreateUnknown
					}
					expected = append(expected, publication{key, e.Inputs[1], state})
				}
				if e.Operation == "broker.PreviewBroker.wait" {
					var key string
					_ = json.Unmarshal(e.Inputs[0], &key)
					r := results[e.ID]
					if r.Completion == "returned" {
						box.data[key] = oracleJSON(r.Value)
					}
				}
			}
			submitted := 0
			box.onCreate = func(key string, data []byte) {
				if submitted >= len(expected) {
					t.Fatalf("unexpected effect %s", key)
				}
				want := expected[submitted]
				submitted++
				if key != want.key || !reflect.DeepEqual(semantic(data), semantic(want.value)) {
					t.Fatalf("MUTATION: oracle-broker-order %s\ngot %.800s\nwant %.800s", key, data, want.value)
				}
				box.result = want.outcome
			}
			active := map[int]bool{}
			checked := 0
			for _, e := range fixture.Events {
				if e.Kind == "result" {
					delete(active, e.ID)
					continue
				}
				if e.Kind != "request" || !strings.HasPrefix(e.Operation, "broker.PreviewBroker.") {
					continue
				}
				method := strings.TrimPrefix(e.Operation, "broker.PreviewBroker.")
				if method == "write" || method == "wait" || method == "register" {
					continue
				}
				if len(active) > 0 {
					continue
				}
				// Raw collect's read callback is absent in this frozen export. Retain its
				// explicit pending scope instead of manufacturing a raw report from output.
				if method == "collect" {
					continue
				}
				active[e.ID] = true
				end := time.UnixMilli(m.Deadline)
				var got any
				var callErr error
				switch method {
				case "start", "prepare":
					var token string
					_ = json.Unmarshal(e.Inputs[0], &token)
					got, callErr = b.Prepare(context.Background(), token, end)
					if method == "start" && callErr == nil {
						got, callErr = b.Begin(context.Background(), end)
					}
				case "begin":
					got, callErr = b.Begin(context.Background(), end)
				case "arm":
					var operation string
					var ids []callharness.OperationIdentity
					_ = json.Unmarshal(e.Inputs[0], &operation)
					_ = json.Unmarshal(e.Inputs[1], &ids)
					callErr = b.Arm(context.Background(), operation, ids, end)
				default:
					t.Fatalf("unhandled broker method %s", method)
				}
				want := results[e.ID]
				if (callErr != nil) != (want.Completion == "threw") {
					t.Fatalf("%s: got %v want %s", method, callErr, want.Completion)
				}
				if callErr == nil && got != nil {
					raw, _ := json.Marshal(got)
					if !reflect.DeepEqual(semantic(raw), semantic(want.Value)) {
						t.Fatalf("%s result differs", method)
					}
				}
				checked++
			}
			if checked == 0 || submitted != len(expected) {
				t.Fatalf("replay incomplete: %d methods, %d/%d effects", checked, submitted, len(expected))
			}
		})
	}
}
func oracleJSON(raw []byte) []byte {
	var v any
	_ = json.Unmarshal(raw, &v)
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, y := range x {
				if absent, ok := y.(map[string]any); ok && len(absent) == 1 && absent["absent"] == true {
					delete(x, k)
				} else {
					x[k] = clean(y)
				}
			}
			return x
		case []any:
			for i, y := range x {
				x[i] = clean(y)
			}
			return x
		default:
			return v
		}
	}
	out, _ := json.Marshal(clean(v))
	return out
}
func semantic(raw []byte) any {
	var v any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return "invalid-json"
	}
	var canonical func(any) any
	canonical = func(v any) any {
		switch x := v.(type) {
		case string:
			if at, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return at.UTC().Format(time.RFC3339Nano)
			}
			return x
		case []any:
			for i := range x {
				x[i] = canonical(x[i])
			}
			return x
		case map[string]any:
			for k, y := range x {
				x[k] = canonical(y)
			}
			return x
		default:
			return v
		}
	}
	return canonical(v)
}
func TestOracleCallerManifests(t *testing.T) {
	corpus, err := operatorgates.LoadCorpus("../operatorgates/testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for name := range corpus {
		if strings.Contains(name, "/") {
			continue
		}
		fixture, err := corpus.Fixture(name)
		if err != nil {
			t.Fatal(err)
		}
		requests := map[int]operatorgates.OracleEvent{}
		for _, e := range fixture.Events {
			if e.Operation != "broker.validatePreviewManifest" {
				continue
			}
			if e.Kind == "request" {
				requests[e.ID] = e
				continue
			}
			if e.Kind != "result" {
				continue
			}
			req := requests[e.ID]
			var m operatorcaller.Manifest
			var i operatorcaller.Identity
			var now int64
			if len(req.Inputs) != 3 || json.Unmarshal(req.Inputs[0], &m) != nil || json.Unmarshal(req.Inputs[1], &i) != nil || json.Unmarshal(req.Inputs[2], &now) != nil {
				t.Fatal("manifest fixture input")
			}
			err := m.Validate(i, time.UnixMilli(now))
			if (err != nil) != (e.Completion == "threw") {
				t.Fatalf("%s event %d: %v vs %s", name, e.ID, err, e.Completion)
			}
			count++
		}
	}
	if count == 0 {
		t.Fatal("no manifests")
	}
	t.Log(fmt.Sprint("manifest assertions: ", count))
}

type testClock struct{ at time.Time }

func (c *testClock) Now() time.Time { return c.at }
func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.at = c.at.Add(d)
	return ctx.Err()
}

type testFence struct{ latched bool }

func (f *testFence) Latched() bool { return f.latched }
func (f *testFence) Halt(string)   { f.latched = true }

type memoryBox struct {
	data     map[string][]byte
	result   operatorcaller.CreateState
	onCreate func(string, []byte)
}

func (m *memoryBox) Read(_ context.Context, key string) (operatorcaller.ReadResult, error) {
	b, ok := m.data[key]
	return operatorcaller.ReadResult{Found: ok, Data: b}, nil
}
func (m *memoryBox) Create(_ context.Context, key string, b []byte) operatorcaller.CreateState {
	if m.onCreate != nil {
		m.onCreate(key, b)
	}
	if m.result != operatorcaller.Committed {
		return m.result
	}
	if _, ok := m.data[key]; ok {
		return operatorcaller.Conflict
	}
	m.data[key] = b
	return operatorcaller.Committed
}
