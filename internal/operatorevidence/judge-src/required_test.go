package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/callharness"
)

// Exercise every reachable struct, including optional diagnostic containers,
// using a control that still passes the unchanged judges.
func completePoolFixture(t *testing.T) previewPoolInput {
	t.Helper()
	input := rawPoolSample()
	for i := range input.Phases {
		phase := &input.Phases[i]
		for j := range phase.Receipts {
			phase.Receipts[j].Result.Result.DuplicateWindows = []agent.DuplicateWindow{{}}
		}
		var stream strings.Builder
		encoder := json.NewEncoder(&stream)
		decoder := json.NewDecoder(strings.NewReader(phase.CallerJSONL))
		for decoder.More() {
			var m loadgen.Message
			if err := decoder.Decode(&m); err != nil {
				t.Fatal(err)
			}
			if m.Stats != nil {
				m.Stats.RuntimeMemory = &loadgen.RuntimeMemory{}
			}
			if c := m.Call; c != nil {
				c.Recording.Delivery = &callharness.WholeCallDelivery{}
				c.Answer.Media = []callharness.AnswerMedia{{}}
				c.ConnectionStates = []callharness.StateChange{{}}
				c.SRTPDiagnostics = &callharness.SRTPDiagnosticReport{Samples: []callharness.SRTPFailureSample{{}}}
				c.Distributed.ObservedRate = &callharness.ClockRateInterval{}
				c.Tracks[0].MediaGaps = []callharness.MediaGap{{}}
				c.Tracks[0].Arrivals = []time.Duration{0}
				c.Tracks[1].Video = &callharness.VideoReport{}
				for j := range c.Moves {
					v := &c.Moves[j]
					r := phase.Receipts[j]
					v.Receipt = &callharness.DistributedEvent{Identity: r.Identity, Kind: r.Kind, Start: r.Start, End: r.End, Result: r.Result, Uncertain: r.Uncertain}
					v.Result.DuplicateWindows = []agent.DuplicateWindow{{}}
					v.Tracks = []callharness.MoveTrackReport{{}}
					v.Measurement.ContentLoss.Baselines = []callharness.ContentLossWindow{{}}
				}
				dial := loadgen.Message{Type: "dial", PID: m.PID, Session: m.Session, CallID: m.CallID, Dial: &callharness.DialReport{Outcome: callharness.DialConnected, Session: m.Session, CandidatePairs: []callharness.CandidatePairChecks{{}}}}
				if err := encoder.Encode(dial); err != nil {
					t.Fatal(err)
				}
			}
			if err := encoder.Encode(m); err != nil {
				t.Fatal(err)
			}
		}
		phase.CallerJSONL = stream.String()
	}
	return input
}

func TestDurationBoundRequiredMembers(t *testing.T) {
	t.Parallel()
	control := rawPoolSample()
	if got := fixturePoolVerdict(&control); got.Status != "passed" {
		t.Fatal("passing control", got)
	}
	for _, name := range []string{"resume", "kill_to_live"} {
		for _, invalid := range []string{`{}`, `null`, `{"lower_ns":1000000000}`, `{"upper_ns":1000000000}`} {
			t.Run(name+invalid, func(t *testing.T) {
				t.Parallel()
				for _, failure := range []string{"", "receipt", "counter"} {
					input := rawPoolSample()
					if failure == "receipt" {
						input.Phases[0].Receipts[0].Result.Lost = true
					}
					if failure == "counter" {
						replaceCall(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
					}
					changed := false
					for i := range input.Phases {
						old := `"` + name + `":{"lower_ns":1000000000,"upper_ns":1000000000}`
						next := strings.Replace(input.Phases[i].CallerJSONL, old, `"`+name+`":`+invalid, 1)
						changed = changed || next != input.Phases[i].CallerJSONL
						input.Phases[i].CallerJSONL = next
					}
					if !changed {
						t.Fatal("bound mutation did not apply")
					}
					got := fixturePoolVerdict(&input)
					want := "inconclusive"
					if failure != "" {
						want = "failed"
					}
					if got.Status != want {
						t.Fatal("MUTATION: required-duration-members", name, invalid, failure, got)
					}
				}
			})
		}
	}
}

type requiredSite struct {
	owner  reflect.Type
	field  reflect.StructField
	object map[string]any
	name   string
}

func requiredSites(value any, typ reflect.Type, seen map[string]requiredSite, types map[reflect.Type]bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if value == nil || reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		types[typ] = true
		object := value.(map[string]any)
		for _, f := range reflect.VisibleFields(typ) {
			if f.PkgPath != "" || f.Tag.Get("json") == "-" {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				name = f.Name
			}
			key := typ.String() + "." + f.Name
			if observe.EvidenceOptional(typ, f, object) == "" {
				if _, ok := seen[key]; !ok {
					seen[key] = requiredSite{typ, f, object, name}
				}
			}
			requiredSites(object[name], f.Type, seen, types)
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return
		}
		for _, v := range value.([]any) {
			requiredSites(v, typ.Elem(), seen, types)
		}
	case reflect.Map:
		object := value.(map[string]any)
		for _, key := range sortedSweepKeys(object) {
			requiredSites(object[key], typ.Elem(), seen, types)
		}
	}
}

// Short mode uses a stable 1/32 field sample, every required optional-ledger
// context, and every named mutation target. Full mode exercises every field.
type fieldSweep struct {
	short     bool
	mandatory map[string]bool
	selected  map[string]bool
}

func newFieldSweep(t *testing.T) *fieldSweep {
	t.Helper()
	s := &fieldSweep{short: testing.Short(), mandatory: map[string]bool{}, selected: map[string]bool{}}
	if s.short {
		raw, err := os.ReadFile("../../../scripts/ci/gate-mutation/cases.json")
		if err != nil {
			t.Fatal(err)
		}
		var cases []struct{ Test string }
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if suffix, ok := strings.CutPrefix(c.Test, t.Name()+"/"); ok {
				key, ok := strings.CutSuffix(suffix, "/delete")
				if ok {
					s.mandatory[key] = true
					continue
				}
				if key, ok := strings.CutSuffix(suffix, "/null"); ok {
					s.mandatory[key] = true
				}
			}
		}
	}
	t.Cleanup(func() {
		for key := range s.mandatory {
			if !s.selected[key] {
				t.Errorf("named mutation target omitted from PR sample: %s", key)
			}
		}
		t.Logf("field sweep: %d selected fields/contexts (short=%t)", len(s.selected), s.short)
	})
	return s
}

func (s *fieldSweep) include(key string, requiredContext bool) bool {
	hash := sha256.Sum256([]byte(key))
	if !s.short || hash[0]%32 == 0 || requiredContext || s.mandatory[key] {
		s.selected[key] = true
		return true
	}
	return false
}

func sortedSweepKeys[V any](sites map[string]V) []string {
	keys := make([]string, 0, len(sites))
	for key := range sites {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Named regressions frequently share identical controls. Reuse only an exact
// serialized pool input; changed raw JSONL, provenance, receipts or counters
// always get a fresh judgment. Composed and pinned comparisons bypass this memo.
var fixturePoolVerdicts sync.Map

func fixturePoolVerdict(input *previewPoolInput) previewMediaSummary {
	raw, err := json.Marshal(input)
	if err != nil {
		return pooledPreviewMedia(input)
	}
	key := sha256.Sum256(raw)
	compute := sync.OnceValue(func() previewMediaSummary { return pooledPreviewMedia(input) })
	value, _ := fixturePoolVerdicts.LoadOrStore(key, compute)
	return value.(func() previewMediaSummary)()
}

// Reuse only identical product values at the same document boundary in the same
// fixture. Include both raw presence contracts: the main decoder and independent
// gap-failure recovery. Every wire mutation still traverses both from its root.
type sweepVerdicts struct{ entries sync.Map }

func (v *sweepVerdicts) judge(t *testing.T, fixture string, input previewPoolInput, doc optionalDocument, raw []byte, decoded any, contractErr error) previewMediaSummary {
	t.Helper()
	gapErr := observe.EvidenceTypeFields(raw, doc.typ, reflect.TypeFor[callharness.DistributedGap]())
	typed, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("%s/%s/%d/%d/%t/%t/%x", fixture, doc.typ, doc.phase, doc.line, contractErr != nil, gapErr != nil, sha256.Sum256(typed))
	compute := sync.OnceValue(func() previewMediaSummary {
		changed := optionalInput(t, input, doc, raw)
		return fixturePoolVerdict(&changed)
	})
	value, _ := v.entries.LoadOrStore(key, compute)
	return value.(func() previewMediaSummary)()
}

func TestEveryRequiredEvidenceMember(t *testing.T) {
	t.Parallel()
	sweep := newFieldSweep(t)
	input := completePoolFixture(t)
	if got := fixturePoolVerdict(&input); got.Status != "passed" {
		t.Fatal("expanded passing control", got)
	}
	// Decode each real JSONL variant separately; callerJsonl is a product string,
	// so its contents have their own typed boundary.
	var documents []struct {
		value       any
		typ         reflect.Type
		phase, line int
	}
	data, _ := json.Marshal(input)
	var poolObject any
	_ = json.Unmarshal(data, &poolObject)
	documents = append(documents, struct {
		value       any
		typ         reflect.Type
		phase, line int
	}{poolObject, reflect.TypeFor[previewPoolInput](), -1, -1})
	for i, p := range input.Phases {
		for j, line := range strings.Split(strings.TrimSpace(p.CallerJSONL), "\n") {
			var object any
			_ = json.Unmarshal([]byte(line), &object)
			documents = append(documents, struct {
				value       any
				typ         reflect.Type
				phase, line int
			}{object, reflect.TypeFor[loadgen.Message](), i, j})
		}
	}
	var verdicts sweepVerdicts
	seen := map[string]bool{}
	types := map[reflect.Type]bool{}
	for _, doc := range documents {
		sites := map[string]requiredSite{}
		requiredSites(doc.value, doc.typ, sites, types)
		for _, key := range sortedSweepKeys(sites) {
			site := sites[key]
			if seen[key] {
				continue
			}
			seen[key] = true
			if !sweep.include(key, false) {
				continue
			}
			for _, mode := range []string{"delete", "null"} {
				t.Run(key+"/"+mode, func(t *testing.T) {
					original, exists := site.object[site.name]
					if !exists || original == nil {
						t.Fatal("control lacks required field", key)
					}
					if mode == "delete" {
						delete(site.object, site.name)
					} else {
						site.object[site.name] = nil
					}
					raw, _ := json.Marshal(doc.value)
					site.object[site.name] = original
					t.Parallel()
					// Every mutation crosses the real document root, including all
					// containing pointers, slices and maps, before product decoding.
					destination := reflect.New(doc.typ).Interface()
					err := observe.DecodeEvidenceLimit(raw, destination, 64<<20)
					if !observe.IsIncompleteEvidence(err) {
						t.Fatal("MUTATION: recursive-required-field", key, mode, err)
					}
					root := optionalDocument{doc.value, doc.typ, doc.phase, doc.line}
					if got := verdicts.judge(t, "required", input, root, raw, destination, err); got.Status == "passed" {
						t.Fatal("MUTATION: recursive-required-field", key, mode, got)
					}
				})
			}
		}
	}
	// The recursion must reach every struct in the input type graph, including
	// optional containers populated by completePoolFixture.
	visited := map[reflect.Type]bool{}
	var check func(reflect.Type)
	check = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if visited[typ] || typ.Kind() != reflect.Struct || reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
			return
		}
		visited[typ] = true
		if !types[typ] {
			t.Error("unexercised consumed struct", typ)
		}
		for _, f := range reflect.VisibleFields(typ) {
			if f.PkgPath == "" && f.Tag.Get("json") != "-" {
				check(f.Type)
			}
		}
	}
	check(reflect.TypeFor[previewPoolInput]())
	check(reflect.TypeFor[loadgen.Message]())
	t.Logf("%d required struct fields across %d consumed struct types; delete and null each", len(seen), len(types))
}

func TestRequiredFieldsRetainIndependentGapFailure(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	editPoolCalls(t, &input, func(c *callharness.Report) {
		for i := range c.Moves {
			for j := range c.Moves[i].Distributed.Gaps {
				c.Moves[i].Distributed.Gaps[j].EventGap = callharness.DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
			}
		}
	})
	input.Phases[0].CallerJSONL = strings.Replace(input.Phases[0].CallerJSONL, `"resume":{"lower_ns":1000000000,"upper_ns":1000000000}`, `"resume":{}`, 1)
	input.Phases[1].CallerJSONL = strings.Replace(input.Phases[1].CallerJSONL, `"cpu_percent_per_gomaxprocs":0,`, "", 1)
	if got := fixturePoolVerdict(&input); got.Status != "failed" {
		t.Fatal("MUTATION: required-retains-independent-gaps", got)
	}
}

func TestEveryRequiredManifestMember(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	line := strings.Split(input.Phases[0].CallerJSONL, "\n")[0]
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "stats"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stats", "00000.json"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(line))
	entries := []statsManifestEntry{{Name: "00000.json", SHA: fmt.Sprintf("%x", sum), Bytes: len(line)}}
	raw, _ := json.Marshal(entries)
	name := filepath.Join(dir, "stats-manifest.json")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSamples(dir, 42, true); err != nil {
		t.Fatal("passing manifest", err)
	}
	var object any
	_ = json.Unmarshal(raw, &object)
	sites := map[string]requiredSite{}
	requiredSites(object, reflect.TypeFor[[]statsManifestEntry](), sites, map[reflect.Type]bool{})
	for key, site := range sites {
		for _, mode := range []string{"delete", "null"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				original := site.object[site.name]
				if mode == "delete" {
					delete(site.object, site.name)
				} else {
					site.object[site.name] = nil
				}
				changed, _ := json.Marshal(object)
				site.object[site.name] = original
				if err := os.WriteFile(name, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := loadSamples(dir, 42, true); err == nil {
					t.Fatal("MUTATION: recursive-manifest-fields", key, mode)
				}
			})
		}
	}
}

func TestRequiredFieldsRetainIndependentFailures(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"started-time", "video-expected"} {
		input := rawPoolSample()
		replaceCall(t, &input, func(c *callharness.Report) {
			if invalid == "started-time" {
				c.DecryptionFailures.AuthTag = 1
			} else {
				c.Moves[0].Measurement.VideoLoss = callharness.MetricVerdict{Trusted: true, Failed: true}
			}
		})
		if invalid == "started-time" {
			input.Phases[0].CallerJSONL = strings.Replace(input.Phases[0].CallerJSONL, `"StartedAt":"2026-10-25T00:00:00Z"`, `"StartedAt":{}`, 1)
		} else {
			input.Phases[0].CallerJSONL = strings.Replace(input.Phases[0].CallerJSONL, `"VideoExpected":true,`, "", 1)
		}
		if got := fixturePoolVerdict(&input); got.Status != "failed" {
			t.Fatal("MUTATION: required-independent-failure", invalid, got)
		}
	}
}
