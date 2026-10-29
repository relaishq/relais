package main

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestPooledVideoEvidenceCannotDisappear(t *testing.T) {
	t.Parallel()
	control := rawPoolSample()
	if got := fixturePoolVerdict(&control); got.Status != "passed" {
		t.Fatal("passing control", got)
	}
	replaceCall(t, &control, func(c *callharness.Report) { c.Tracks[1].Video.OnlineDecode.Errors = 1 })
	if got := fixturePoolVerdict(&control); got.Status == "passed" {
		t.Fatal("decoder errors did not prevent pass", got)
	}
	t.Run("typed", func(t *testing.T) {
		typed := previewSample()
		typed.Processes[0].Calls[0].Call.Tracks[1].Video = nil
		if got := previewMedia(&typed); got.Status == "passed" {
			t.Fatal("missing typed video evidence passed", got)
		}
	})

	for _, mode := range []string{"delete", "null"} {
		t.Run(mode, func(t *testing.T) {
			input := control
			input.Phases = append([]previewMediaInput{}, control.Phases...)
			var stream strings.Builder
			encoder := json.NewEncoder(&stream)
			decoder := json.NewDecoder(strings.NewReader(input.Phases[0].CallerJSONL))
			changed := false
			for decoder.More() {
				var object map[string]any
				if err := decoder.Decode(&object); err != nil {
					t.Fatal(err)
				}
				if object["type"] == "call" {
					call := object["call"].(map[string]any)
					for _, raw := range call["Tracks"].([]any) {
						track := raw.(map[string]any)
						if track["Kind"] == "video" {
							if track["Video"].(map[string]any)["OnlineDecode"].(map[string]any)["Errors"] != float64(1) {
								t.Fatal("missing failing witness")
							}
							if mode == "delete" {
								delete(track, "Video")
							} else {
								track["Video"] = nil
							}
							changed = true
						}
					}
				}
				if err := encoder.Encode(object); err != nil {
					t.Fatal(err)
				}
			}
			if !changed {
				t.Fatal("video mutation did not apply")
			}
			input.Phases[0].CallerJSONL = stream.String()
			if got := fixturePoolVerdict(&input); got.Status == "passed" {
				t.Fatal("MUTATION: video-evidence-required", mode, got)
			}
			input.Phases[0].Receipts[0].Result.Lost = true
			if got := fixturePoolVerdict(&input); got.Status != "failed" {
				t.Fatal("video absence erased independent failure", got)
			}
		})
	}
}

type optionalDocument struct {
	value       any
	typ         reflect.Type
	phase, line int
}

func optionalDocuments(t *testing.T, input previewPoolInput) []optionalDocument {
	t.Helper()
	data, _ := json.Marshal(input)
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	docs := []optionalDocument{{value, reflect.TypeFor[previewPoolInput](), -1, -1}}
	// Both ingress representations have ledger entries. They share the real
	// wire format, so every alias is tested through the same pooled judge.
	var alias any
	_ = json.Unmarshal(data, &alias)
	docs = append(docs, optionalDocument{alias, reflect.TypeFor[operatorevidence.Pool](), -1, -1})
	for i, phase := range input.Phases {
		for j, line := range strings.Split(strings.TrimSpace(phase.CallerJSONL), "\n") {
			var object any
			if err := json.Unmarshal([]byte(line), &object); err != nil {
				t.Fatal(err)
			}
			docs = append(docs, optionalDocument{object, reflect.TypeFor[loadgen.Message](), i, j})
		}
	}
	return docs
}

// Record every ledger field in every encountered discriminator context. Prefer a
// supplied non-null object over an empty optional slot: selecting only the first
// audio track would hide removal of the failing video track in the old ledger.
func optionalSites(value any, typ reflect.Type, sites map[string]requiredSite, ledger map[string]string, parentKind string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if value == nil || reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		object := value.(map[string]any)
		if kind, ok := object["Kind"].(string); ok {
			parentKind = kind
		}
		for _, field := range reflect.VisibleFields(typ) {
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				embedded := field.Type
				for embedded.Kind() == reflect.Pointer {
					embedded = embedded.Elem()
				}
				if field.Anonymous && embedded.Kind() == reflect.Struct {
					continue
				}
				name = field.Name
			}
			if _, listed := ledger[typ.String()+"."+field.Name]; listed {
				if _, ok := object[name]; !ok {
					raw, _ := json.Marshal(reflect.Zero(field.Type).Interface())
					_ = json.Unmarshal(raw, &value)
					object[name] = value
				}
				key := typ.String() + "." + field.Name
				for _, discriminator := range []string{"type", "Kind", "kind"} {
					if tag, ok := object[discriminator].(string); ok {
						key += "/" + tag
					}
				}
				// This bound's applicability belongs to its enclosing move.
				if typ == reflect.TypeFor[callharness.DistributedMeasurement]() && field.Name == "KillToLive" {
					key += "/" + parentKind
				}
				prior, seen := sites[key]
				if !seen || prior.object[prior.name] == nil && object[name] != nil {
					sites[key] = requiredSite{typ, field, object, name}
				}
			}
			optionalSites(object[name], field.Type, sites, ledger, parentKind)
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return
		}
		for _, element := range value.([]any) {
			optionalSites(element, typ.Elem(), sites, ledger, parentKind)
		}
	case reflect.Map:
		object := value.(map[string]any)
		for _, key := range sortedSweepKeys(object) {
			optionalSites(object[key], typ.Elem(), sites, ledger, parentKind)
		}
	}
}

func optionalInput(input previewPoolInput, doc optionalDocument, raw []byte) (previewPoolInput, error) {
	if doc.phase < 0 {
		return decodePool(raw)
	}
	changed := input
	changed.Phases = append([]previewMediaInput{}, input.Phases...)
	lines := strings.Split(strings.TrimSpace(changed.Phases[doc.phase].CallerJSONL), "\n")
	lines[doc.line] = string(raw)
	changed.Phases[doc.phase].CallerJSONL = strings.Join(lines, "\n") + "\n"
	return changed, nil
}

func TestEveryOptionalEvidenceMember(t *testing.T) {
	t.Parallel()
	sweep := newFieldSweep(t)
	passing := completePoolFixture(t)
	input := passing
	input.Phases = append([]previewMediaInput{}, passing.Phases...)
	if got := fixturePoolVerdict(&input); got.Status != "passed" {
		t.Fatal("passing control", got)
	}
	// This is the real failing parent that exposed the ledger defect, rather
	// than an unrelated failure sentinel that would mask removal of Video.
	replaceCall(t, &input, func(c *callharness.Report) { c.Tracks[1].Video.OnlineDecode.Errors = 1 })
	if got := fixturePoolVerdict(&input); got.Status == "passed" {
		t.Fatal("failing control", got)
	}
	ledger := observe.EvidenceOptionalLedger()
	covered := map[string]bool{}
	contexts := map[string]bool{}
	var verdicts sweepVerdicts
	passingDocs := optionalDocuments(t, passing)
	for index, doc := range optionalDocuments(t, input) {
		passingDoc := passingDocs[index]
		passingSites := map[string]requiredSite{}
		optionalSites(passingDoc.value, passingDoc.typ, passingSites, ledger, "")
		sites := map[string]requiredSite{}
		optionalSites(doc.value, doc.typ, sites, ledger, "")
		baselineRaw, _ := json.Marshal(doc.value)
		controlBaselineRaw, _ := json.Marshal(passingDoc.value)
		baseline := reflect.New(doc.typ).Interface()
		controlBaseline := reflect.New(doc.typ).Interface()
		if err := observe.DecodeEvidenceLimit(baselineRaw, baseline, 64<<20); err != nil {
			t.Fatal(err)
		}
		if err := observe.DecodeEvidenceLimit(controlBaselineRaw, controlBaseline, 64<<20); err != nil {
			t.Fatal(err)
		}
		for _, key := range sortedSweepKeys(sites) {
			site := sites[key]
			if contexts[key] {
				continue
			}
			contexts[key] = true
			covered[site.owner.String()+"."+site.field.Name] = true
			optional := observe.EvidenceOptional(site.owner, site.field, site.object) != ""
			if !sweep.include(key, !optional) {
				continue
			}
			for _, mode := range []string{"delete", "null"} {
				t.Run(key+"/"+mode, func(t *testing.T) {
					original, exists := site.object[site.name]
					if !exists {
						t.Fatal("optional field not materialized", key)
					}
					if mode == "delete" {
						delete(site.object, site.name)
					} else {
						site.object[site.name] = nil
					}
					raw, _ := json.Marshal(doc.value)
					site.object[site.name] = original
					controlSite := passingSites[key]
					controlOriginal := controlSite.object[controlSite.name]
					if mode == "delete" {
						delete(controlSite.object, controlSite.name)
					} else {
						controlSite.object[controlSite.name] = nil
					}
					controlRaw, _ := json.Marshal(passingDoc.value)
					controlSite.object[controlSite.name] = controlOriginal
					t.Parallel()
					// Exercise the alias's actual contract as well as the production pooled
					// entry. Required contexts must reject the mutation too; a ledger exception
					// in another context cannot excuse it.
					destination := reflect.New(doc.typ).Interface()
					err := observe.DecodeEvidenceLimit(raw, destination, 64<<20)
					if !optional && !observe.IsIncompleteEvidence(err) {
						t.Fatal("MUTATION: optional-evidence-cannot-restore-pass", "required context must be incomplete", key, err)
					}
					if optional && err != nil {
						t.Fatal("optional context rejected", key, err)
					}
					controlDestination := reflect.New(doc.typ).Interface()
					controlErr := observe.DecodeEvidenceLimit(controlRaw, controlDestination, 64<<20)
					if !optional && !observe.IsIncompleteEvidence(controlErr) {
						t.Fatal("required context accepted on passing control", key, controlErr)
					}
					if optional && controlErr != nil {
						t.Fatal("optional passing control rejected", key, controlErr)
					}
					// Full-root equality, not an immediate-owner projection, is the
					// only shortcut. Required contexts still reach pooled quarantine.
					control := previewMediaSummary{Status: "passed"}
					if !optional || !reflect.DeepEqual(controlDestination, controlBaseline) {
						var judgeErr error
						control, judgeErr = verdicts.judge(t, "passing", passing, doc, controlRaw, controlDestination, controlErr)
						if judgeErr != nil {
							t.Fatal("optional passing input rejected", key, judgeErr)
						}
					}
					semanticRequired := key == "callharness.DistributedMeasurement.KillToLive/takeover" || key == "mediaworker.HandoverResult.DuplicateWindows"
					if optional && !semanticRequired && control.Status != "passed" {
						t.Fatal("optional omission changed passing control", key, control)
					} else if (!optional || semanticRequired) && control.Status != "inconclusive" {
						t.Fatal("MUTATION: optional-evidence-cannot-restore-pass", "required omission did not quarantine passing control", key, control)
					}
					changed := previewMediaSummary{Status: "inconclusive"}
					if !optional || !reflect.DeepEqual(destination, baseline) {
						var judgeErr error
						changed, judgeErr = verdicts.judge(t, "failing", input, doc, raw, destination, err)
						if judgeErr != nil {
							t.Fatal("optional failing input rejected", key, judgeErr)
						}
					}
					if changed.Status == "passed" {
						t.Fatal("MUTATION: optional-evidence-cannot-restore-pass", key, mode, changed)
					}
				})
			}
		}
	}
	for key, reason := range ledger {
		if reason == "" || !covered[key] {
			t.Error("untested optional ledger entry", key)
		}
	}
	for key := range covered {
		if _, ok := ledger[key]; !ok {
			t.Error("unlisted optional exception", key)
		}
	}
	t.Logf("%d optional ledger fields in %d discriminator contexts; delete and null each", len(covered), len(contexts))
}

func customEvidenceDecoders(roots ...reflect.Type) map[reflect.Type]bool {
	visited := map[reflect.Type]bool{}
	custom := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		if visited[typ] {
			return
		}
		visited[typ] = true
		if typ.Implements(reflect.TypeFor[json.Unmarshaler]()) || typ.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
			for typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
			custom[typ] = true
			return
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		case reflect.Map:
			walk(typ.Key())
			walk(typ.Elem())
		case reflect.Struct:
			for _, field := range reflect.VisibleFields(typ) {
				if field.PkgPath == "" && field.Tag.Get("json") != "-" {
					walk(field.Type)
				}
			}
		}
	}
	for _, typ := range roots {
		walk(typ)
	}
	return custom
}

func TestCustomEvidenceDecoderInventory(t *testing.T) {
	t.Parallel()
	custom := customEvidenceDecoders(reflect.TypeFor[previewPoolInput](), reflect.TypeFor[loadgen.Message](), reflect.TypeFor[statsManifestEntry](), reflect.TypeFor[operatorevidence.Pool](), reflect.TypeFor[operatorevidence.Summary]())
	if len(custom) != 1 || !custom[reflect.TypeFor[time.Time]()] {
		t.Fatal("MUTATION: custom-decoder-inventory", custom)
	}
	t.Log("time.Time only; scalar presence/shape/format covered by observe.TestTimeEvidenceScalarContract; excluded from struct-member counts")
}

// Synthetic additions prove that inventory sees decoder methods before
// unwrapping named containers, and sees text decoders on map keys.
type inventoryKey string
type inventorySlice []int
type inventoryArray [1]int
type inventoryMap map[string]int

func (*inventoryKey) UnmarshalText([]byte) error   { return nil }
func (*inventorySlice) UnmarshalJSON([]byte) error { return nil }
func (*inventoryArray) UnmarshalJSON([]byte) error { return nil }
func (*inventoryMap) UnmarshalJSON([]byte) error   { return nil }

func TestCustomEvidenceDecoderInventoryTraversal(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{reflect.TypeFor[inventorySlice](), reflect.TypeFor[inventoryArray](), reflect.TypeFor[inventoryMap](), reflect.TypeFor[inventoryKey]()} {
		root := reflect.PointerTo(reflect.SliceOf(reflect.MapOf(reflect.TypeFor[inventoryKey](), typ)))
		custom := customEvidenceDecoders(root)
		if !custom[typ] || !custom[reflect.TypeFor[inventoryKey]()] {
			t.Fatal("MUTATION: custom-decoder-inventory-traversal", typ, custom)
		}
	}
}

func TestOptionalMirroredDiagnostics(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"delete", "null"} {
		t.Run(mode, func(t *testing.T) {
			input := completePoolFixture(t)
			if got := fixturePoolVerdict(&input); got.Status != "passed" {
				t.Fatal("passing control", got)
			}
			// Omit the diagnostic from both source and copied receipts. Mutation
			// starts with supplied nonempty inventories, not an already absent field.
			changed := 0
			var omit func(any)
			omit = func(value any) {
				switch value := value.(type) {
				case map[string]any:
					if _, found := value["DuplicateWindows"]; found {
						if mode == "delete" {
							delete(value, "DuplicateWindows")
						} else {
							value["DuplicateWindows"] = nil
						}
						changed++
					}
					for _, child := range value {
						omit(child)
					}
				case []any:
					for _, child := range value {
						omit(child)
					}
				}
			}
			// JSONL is a separate typed boundary, so mutate it before the envelope.
			docs := optionalDocuments(t, input)
			for _, doc := range docs[2:] {
				omit(doc.value)
				raw, _ := json.Marshal(doc.value)
				if err := observe.DecodeEvidenceLimit(raw, reflect.New(doc.typ).Interface(), 64<<20); err != nil {
					t.Fatal(err)
				}
				var err error
				input, err = optionalInput(input, doc, raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			doc := optionalDocuments(t, input)[0]
			omit(doc.value)
			raw, _ := json.Marshal(doc.value)
			var err error
			input, err = optionalInput(input, doc, raw)
			if err != nil {
				t.Fatal(err)
			}
			if changed == 0 {
				t.Fatal("no diagnostic omissions applied")
			}
			if got := fixturePoolVerdict(&input); got.Status != "passed" {
				t.Fatal("consistent optional omission", mode, got)
			}
		})
	}
}
