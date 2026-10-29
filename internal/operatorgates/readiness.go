package operatorgates

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed testdata/coverage.json testdata/mutation-coverage.json testdata/equivalence-deltas.json testdata/transcripts/* testdata/deltas/*
var readinessFiles embed.FS

type readinessFixture struct {
	SchemaVersion                int
	Scenario, ExpectedTestResult string
	Events                       []struct {
		ID              int
		Kind, Operation string
	}
}

type replayCoverage struct {
	SchemaVersion int
	CorpusDigest  string
	FullSuite     string
	Groups        []struct {
		ID, Evidence, Reason string
		Required             bool
		Tests                []string
	}
	Fixtures []struct{ Fixture, Scenario, FullReplay, Reason string }
}
type mutationRecord struct {
	ID, Owner, Evidence, GuardFingerprint, Test, Package, Reason, SuccessorContract, SuccessorMutation string
}
type mutationCoverage struct {
	SchemaVersion                     int
	OriginalCaseCount                 int
	OwnedCases                        []string
	AdditionalProtocolCases           []string
	Pending, Killed, Retired, Private []mutationRecord
}

// Invocation is the immutable request ID, joined to its result in the fixture.
type deltaBinding struct {
	Fixture, Operation string
	Invocation         int
}
type equivalenceDelta struct {
	ID       string
	Bindings []deltaBinding
	Tests    []string
}
type equivalenceChanges struct {
	SchemaVersion   int
	Deltas          []equivalenceDelta
	Representations []equivalenceDelta
}

const originalMutationDigest = "a393b426fc2fa5733e63529415ba8c2b1259f3ec003f8c8a97165ec9c0e93f9f"

// CheckReadiness checks the frozen replay and mutation accounting compiled into
// this executable. Rehearsal and launch admission must call it before effects.
// It proves accounting completeness, not a successful rehearsal or paid run.
func CheckReadiness() error { return checkReadiness(readinessFiles) }

func checkReadiness(source fs.FS) error {
	decode := func(name string, out any) error {
		data, err := fs.ReadFile(source, "testdata/"+name)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	}
	var coverage replayCoverage
	if err := decode("coverage.json", &coverage); err != nil {
		return err
	}
	if coverage.SchemaVersion != 1 {
		return fmt.Errorf("operator-readiness: replay schema")
	}
	if coverage.FullSuite != "accounted" {
		return fmt.Errorf("operator-readiness: replay suite %s", coverage.FullSuite)
	}
	if len(coverage.Groups) == 0 {
		return fmt.Errorf("operator-readiness: missing required groups")
	}
	groups := map[string]bool{}
	for _, g := range coverage.Groups {
		if g.ID == "" || groups[g.ID] || g.Evidence != "replayed" && g.Evidence != "tested" && (g.Required || g.Evidence != "non-replayable" || g.Reason == "") {
			return fmt.Errorf("operator-readiness: replay group %s", g.ID)
		}
		switch g.ID {
		case "core", "observations", "procedures", "caller", "telemetry", "runtime", "composition", "strict-verdict":
			if !g.Required {
				return fmt.Errorf("operator-readiness: required group disabled %s", g.ID)
			}
		}
		if (g.Evidence == "replayed" || g.Evidence == "tested") && len(g.Tests) == 0 {
			return fmt.Errorf("operator-readiness: group has no test %s", g.ID)
		}
		groups[g.ID] = true
	}
	for _, required := range []string{"core", "observations", "procedures", "caller", "telemetry", "runtime", "composition", "strict-verdict"} {
		if !groups[required] {
			return fmt.Errorf("operator-readiness: missing required group %s", required)
		}
	}
	corpusFS, err := fs.Sub(source, "testdata/transcripts")
	if err != nil {
		return err
	}
	corpus, err := loadCorpusFS(corpusFS)
	if err != nil {
		return err
	}
	sums, err := fs.ReadFile(corpusFS, "SHA256SUMS")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(sums)
	if hex.EncodeToString(sum[:]) != coverage.CorpusDigest {
		return fmt.Errorf("operator-readiness: corpus identity")
	}
	fixtures := map[string]readinessFixture{}
	for _, row := range coverage.Fixtures {
		if _, exists := fixtures[row.Fixture]; exists {
			return fmt.Errorf("operator-readiness: duplicate fixture")
		}
		if row.FullReplay != "replayed" && (row.FullReplay != "non-replayable" || row.Reason == "") {
			return fmt.Errorf("operator-readiness: fixture %s", row.Fixture)
		}
		var f readinessFixture
		err := json.Unmarshal(corpus[row.Fixture], &f)
		if err != nil || f.SchemaVersion != 1 || f.ExpectedTestResult != "passed" || f.Scenario != row.Scenario {
			return fmt.Errorf("operator-readiness: fixture identity %s", row.Fixture)
		}
		fixtures[row.Fixture] = f
	}
	for name := range corpus {
		if !strings.Contains(name, "/") {
			if _, exists := fixtures[name]; !exists {
				return fmt.Errorf("operator-readiness: uninventoried fixture %s", name)
			}
		}
	}
	var mutations mutationCoverage
	if err := decode("mutation-coverage.json", &mutations); err != nil {
		return err
	}
	if mutations.SchemaVersion != 1 {
		return fmt.Errorf("operator-readiness: mutation schema")
	}
	if len(mutations.Pending) != 0 {
		return fmt.Errorf("operator-readiness: pending mutation %s", mutations.Pending[0].ID)
	}
	original := map[string]bool{}
	fingerprints := map[string]string{}
	recordOriginal := func(row mutationRecord) error {
		fingerprint, err := hex.DecodeString(row.GuardFingerprint)
		if original[row.ID] || additionalCaseID(row.ID, mutations.AdditionalProtocolCases) || err != nil || len(fingerprint) != 32 {
			return fmt.Errorf("operator-readiness: original guard identity %s", row.ID)
		}
		original[row.ID] = true
		fingerprints[row.ID] = row.GuardFingerprint
		return nil
	}
	additional := map[string]bool{}
	for _, id := range mutations.AdditionalProtocolCases {
		if id == "" || additional[id] {
			return fmt.Errorf("operator-readiness: duplicate additional case")
		}
		additional[id] = true
	}
	killed := map[string]bool{}
	for _, row := range mutations.Killed {
		if row.ID == "" || killed[row.ID] || row.Evidence != "killed" || row.Test == "" || row.Package == "" {
			return fmt.Errorf("operator-readiness: invalid kill %s", row.ID)
		}
		killed[row.ID] = true
		if row.GuardFingerprint != "" {
			if err := recordOriginal(row); err != nil {
				return err
			}
		} else if !additional[row.ID] {
			return fmt.Errorf("operator-readiness: unknown additional kill %s", row.ID)
		}
	}
	owned := map[string]bool{}
	for _, id := range mutations.OwnedCases {
		if owned[id] {
			return fmt.Errorf("operator-readiness: duplicate public case %s", id)
		}
		owned[id] = true
		if !original[id] {
			return fmt.Errorf("operator-readiness: unproved public case %s", id)
		}
	}
	for _, row := range mutations.Retired {
		if original[row.ID] || row.ID == "" || row.Evidence != "retired" || row.Reason == "" || row.SuccessorContract == "" || !killed[row.SuccessorMutation] || row.Test == "" {
			return fmt.Errorf("operator-readiness: invalid retirement %s", row.ID)
		}
		if err := recordOriginal(row); err != nil {
			return err
		}
	}
	for _, row := range mutations.Private {
		if original[row.ID] || row.ID == "" || row.Owner != "private" || row.Evidence != "assigned-private" || row.Reason == "" {
			return fmt.Errorf("operator-readiness: invalid private assignment %s", row.ID)
		}
		if err := recordOriginal(row); err != nil {
			return err
		}
	}
	if mutations.OriginalCaseCount != 414 || len(original) != mutations.OriginalCaseCount || len(killed) != len(mutations.OwnedCases)+len(additional) {
		return fmt.Errorf("operator-readiness: mutation accounting does not balance")
	}
	keys := make([]string, 0, len(fingerprints))
	for id := range fingerprints {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var guards strings.Builder
	for _, id := range keys {
		guards.WriteString(id + " " + fingerprints[id] + "\n")
	}
	identity := sha256.Sum256([]byte(guards.String()))
	if hex.EncodeToString(identity[:]) != originalMutationDigest {
		return fmt.Errorf("operator-readiness: original mutation ledger changed")
	}
	native, err := fs.ReadDir(source, "testdata/deltas")
	if err != nil {
		return err
	}
	for _, entry := range native {
		data, err := fs.ReadFile(source, "testdata/deltas/"+entry.Name())
		if err != nil {
			return err
		}
		var f readinessFixture
		if json.Unmarshal(data, &f) != nil || f.SchemaVersion != 1 || f.ExpectedTestResult != "passed" {
			return fmt.Errorf("operator-readiness: native delta fixture")
		}
		fixtures["deltas/"+entry.Name()] = f
	}
	var changes equivalenceChanges
	if err := decode("equivalence-deltas.json", &changes); err != nil {
		return err
	}
	if changes.SchemaVersion != 1 {
		return fmt.Errorf("operator-readiness: delta schema")
	}
	deltaIDs := map[string]bool{}
	for _, delta := range append(changes.Deltas, changes.Representations...) {
		if delta.ID == "" || deltaIDs[delta.ID] || len(delta.Tests) == 0 || len(delta.Bindings) == 0 {
			return fmt.Errorf("operator-readiness: unbound delta %s", delta.ID)
		}
		deltaIDs[delta.ID] = true
		for _, binding := range delta.Bindings {
			f, exists := fixtures[binding.Fixture]
			requests, results := 0, 0
			for _, e := range f.Events {
				if e.ID == binding.Invocation && e.Operation == binding.Operation {
					if e.Kind == "request" {
						requests++
					}
					if e.Kind == "result" {
						results++
					}
				}
			}
			if !exists || binding.Invocation < 1 || requests != 1 || results != 1 {
				return fmt.Errorf("operator-readiness: invalid delta binding %s", delta.ID)
			}
		}
	}
	return nil
}

func additionalCaseID(id string, additional []string) bool {
	for _, candidate := range additional {
		if candidate == id {
			return true
		}
	}
	return false
}
