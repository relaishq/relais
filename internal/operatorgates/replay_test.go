package operatorgates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/relaishq/relais/pkg/operator"
)

func TestOracleCoreProjections(t *testing.T) {
	c, err := LoadCorpus("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	scenarios, assertions := 0, 0
	for name := range c {
		if strings.Contains(name, "/") {
			continue
		}
		f, err := c.Fixture(name)
		if err != nil {
			t.Fatal(err)
		}
		n, err := ReplayCore(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n > 0 {
			scenarios++
			assertions += n
		}
	}
	if scenarios == 0 || assertions == 0 {
		t.Fatal("no core projections executed")
	}
	t.Logf("core projections: %d scenarios, %d assertions; outer gate algorithms remain pending", scenarios, assertions)
}

// The callback seam is already classified in these fixtures. This exercises
// certainty, effect order and the fence, not Python/product observation parsing.
func TestOracleUnknownEffects(t *testing.T) {
	c, err := LoadCorpus("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0f0eb2dc3f770eb9.json", "1561a3d71d17ec42.json"} {
		f, err := c.Fixture(name)
		if err != nil {
			t.Fatal(err)
		}
		var s *Session
		requests := map[int]operator.Request{}
		checked := 0
		for _, e := range f.Events {
			if e.Kind == "request" && (e.Operation == "trials.workerTrials" || e.Operation == "broker.PreviewBroker.start") {
				s, _ = fixtureSession(t)
			}
			step := e.Operation == "trials.workerTrials.arg0.step"
			start := e.Operation == "broker.PreviewBroker.start"
			if !step && !start {
				continue
			}
			if e.Kind == "request" {
				action := ""
				op := operator.MailboxCreate
				if step {
					op = operator.HostExecute
					_ = json.Unmarshal(e.Inputs[0], &action)
				}
				requests[e.ID] = issue(t, s, op, action)
			}
			if e.Kind == "result" {
				transport := operator.Succeeded
				if e.Completion == "threw" {
					transport = operator.Unknown
				}
				got, err := s.Accept(s.ID, reply(requests[e.ID], transport), transport)
				if err != nil || string(got) != e.Certainty {
					t.Fatalf("%s event %d certainty %s want %s", name, e.ID, got, e.Certainty)
				}
				if got == operator.Unknown {
					if _, err := s.Issue(operator.HostExecute, "shared", "restore", nil, nil, s.Budget.end); err == nil {
						t.Fatal("MUTATION: oracle-unknown-fence")
					}
					checked++
				}
			}
		}
		if checked == 0 {
			t.Fatal("missing unknown seam")
		}
	}
}

func TestOracleInventoryAndPublicContent(t *testing.T) {
	c, err := LoadCorpus("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var coverage struct {
		CorpusDigest string `json:"corpusDigest"`
		FullSuite    string `json:"fullSuite"`
		Fixtures     []struct {
			Fixture    string `json:"fixture"`
			FullReplay string `json:"fullReplay"`
		} `json:"fixtures"`
	}
	if json.Unmarshal(raw, &coverage) != nil || coverage.FullSuite != "pending" {
		t.Fatal("invalid coverage inventory")
	}
	sums, err := os.ReadFile("testdata/transcripts/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(sums)
	if hex.EncodeToString(sum[:]) != coverage.CorpusDigest {
		t.Fatal("frozen corpus identity changed")
	}
	pending := map[string]bool{}
	for _, f := range coverage.Fixtures {
		if pending[f.Fixture] || f.FullReplay != "pending" {
			t.Fatal("invalid pending fixture")
		}
		pending[f.Fixture] = true
	}
	forbidden := regexp.MustCompile(`arn:(aws|aws-cn|aws-us-gov):|/(Users|home)/|/private/tmp/|"[0-9]{12}"`)
	for name, b := range c {
		if forbidden.Match(b) {
			t.Fatalf("forbidden public content in %s", name)
		}
		if !strings.Contains(name, "/") {
			if !pending[name] {
				t.Fatal("uninventoried fixture", name)
			}
			delete(pending, name)
		}
	}
	if len(pending) != 0 {
		t.Fatal("inventory references missing fixtures")
	}
}

// The old multi-launch policy is intentionally removed. Preserve its safety
// projection: readiness can arrive while one accepted start is still pending;
// the late unknown must prevent a final pass and every following effect.
func TestOracleLateUnknown(t *testing.T) {
	c, err := LoadCorpus("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.Fixture("0a95c6d05c17f8d8.json")
	if err != nil {
		t.Fatal(err)
	}
	lateID := 0
	for _, e := range f.Events {
		if e.Kind == "result" && strings.HasSuffix(e.Operation, ".dispatch") && e.Certainty == "unknown" {
			lateID = e.ID
			break
		}
	}
	if lateID == 0 {
		t.Fatal("late oracle result missing")
	}
	s, _ := fixtureSession(t)
	var start operator.Request
	reads := map[int]operator.Request{}
	ready := 0
	for _, e := range f.Events {
		if e.Kind == "request" && e.ID == lateID {
			args, _ := json.Marshal(e.Inputs)
			start, err = s.Issue(operator.CallerStart, "caller", "", args, nil, s.Budget.end)
			if err != nil {
				t.Fatal(err)
			}
		}
		if start.ID == "" {
			continue
		}
		if strings.HasSuffix(e.Operation, ".mailbox.get") {
			if e.Kind == "request" {
				args, _ := json.Marshal(e.Inputs)
				reads[e.ID], err = s.Issue(operator.MailboxRead, "caller", "", args, nil, s.Budget.end)
				if err != nil {
					t.Fatal(err)
				}
			}
			if e.Kind == "result" {
				if _, err := s.Accept(s.ID, reply(reads[e.ID], operator.Succeeded), operator.Succeeded); err != nil {
					t.Fatal(err)
				}
				ready++
				final := s.Final([]operator.GateResult{{Gate: operator.Rollout, Status: operator.Passed}})
				if final.Complete || final.Status == operator.Passed {
					t.Fatal("MUTATION: oracle-pending-final")
				}
			}
		}
		if e.ID == lateID && e.Kind == "result" {
			if ready == 0 || e.Completion != "threw" {
				t.Fatal("oracle ordering changed")
			}
			got, err := s.Accept(s.ID, reply(start, operator.Unknown), operator.Unknown)
			if err != nil || string(got) != e.Certainty || !s.Latched() {
				t.Fatal("MUTATION: oracle-late-unknown")
			}
			if _, err := s.Issue(operator.MailboxCreate, "caller", "", nil, nil, s.Budget.end); err == nil {
				t.Fatal("MUTATION: oracle-late-unknown")
			}
			return
		}
	}
	t.Fatal("late projection never completed")
}

func TestMutationCoverageInventory(t *testing.T) {
	raw, err := os.ReadFile("testdata/mutation-coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		OriginalCaseCount int                                    `json:"originalCaseCount"`
		OwnedCases        []string                               `json:"ownedCases"`
		Pending           []struct{ ID, Owner, Evidence string } `json:"pending"`
	}
	if json.Unmarshal(raw, &ledger) != nil || ledger.OriginalCaseCount != 414 {
		t.Fatal("invalid mutation inventory")
	}
	ids := map[string]bool{}
	for _, id := range ledger.OwnedCases {
		if ids[id] {
			t.Fatal("duplicate owned case")
		}
		ids[id] = true
	}
	for _, row := range ledger.Pending {
		if ids[row.ID] || row.Owner == "" || row.Evidence != "pending" {
			t.Fatal("invalid pending mutation")
		}
		ids[row.ID] = true
	}
	if len(ids) != ledger.OriginalCaseCount || len(ledger.OwnedCases) != 14 {
		t.Fatal("mutation accounting does not balance")
	}
}
