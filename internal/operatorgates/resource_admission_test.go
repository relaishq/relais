package operatorgates

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

func admissionCaller(sessions ...string) *operatorcaller.Broker {
	return &operatorcaller.Broker{
		Manifest:     operatorcaller.Manifest{Plan: operatorcaller.Plan{Calls: len(sessions)}},
		CallerPID:    42,
		Registration: &operatorcaller.Registration{Sessions: sessions},
	}
}

func admissionCallLine(t *testing.T, session string, explicitEmpty bool) string {
	t.Helper()
	// Match recorder.moveReports and recorder.report when no operations were recorded.
	var savedMoves []callharness.MoveReport
	report := &callharness.Report{
		StartedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		HungUpAt:  time.Second,
		Tracks:    []callharness.TrackReport{{Kind: "audio"}},
		Moves:     slices.Clone(savedMoves),
		Distributed: &callharness.DistributedRecording{
			RunID: "boundary", Probes: []callharness.ClockProbe{}, Outcome: "inconclusive",
		},
	}
	if explicitEmpty {
		report.Moves = []callharness.MoveReport{}
		report.Distributed.Armed = []callharness.OperationIdentity{}
	}
	var raw bytes.Buffer
	w := loadgen.NewWriter(&raw)
	w.Write(loadgen.Message{Type: "call", PID: 42, Session: session, Call: report})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	return raw.String()
}

func TestAdmissionMediaCharacterization(t *testing.T) {
	for _, mode := range []string{"valid", "missing-hangup", "decryption-failure", "foreign-session"} {
		t.Run(mode, func(t *testing.T) {
			raw := admissionCallLine(t, "boundary-one", true)
			wantStatus, wantCompleted := operator.Inconclusive, 1
			switch mode {
			case "missing-hangup":
				raw = strings.Replace(raw, `"HungUpAt":1000000000,`, "", 1)
				wantCompleted = 0
			case "decryption-failure":
				raw = strings.Replace(raw, `"AuthTag":0`, `"AuthTag":1`, 1)
				wantStatus = operator.Failure
			case "foreign-session":
				raw = strings.Replace(raw, `"session":"boundary-one"`, `"session":"foreign"`, 1)
				wantCompleted = 0
			}
			summary, err := admissionMedia(raw, admissionCaller("boundary-one"))
			wantError := mode == "missing-hangup" || mode == "foreign-session"
			if (err != nil) != wantError || summary.Status != wantStatus || summary.CompletedCalls != wantCompleted || summary.ExpectedCalls != 1 {
				t.Fatalf("mode=%s summary=%+v error=%v", mode, summary, err)
			}
			if mode == "foreign-session" && !strings.Contains(err.Error(), "precondition-failed:caller-evidence") {
				t.Fatalf("foreign session lost precondition error: %v", err)
			}
		})
	}
}

func TestAdmissionMediaAcceptsProductCallsWithoutOperations(t *testing.T) {
	var raw strings.Builder
	for _, session := range []string{"boundary-one", "boundary-two"} {
		line := admissionCallLine(t, session, false)
		if !strings.Contains(line, `"Moves":null`) || !strings.Contains(line, `"armed":null`) {
			t.Fatal("product fixture must emit null operation slices")
		}
		raw.WriteString(line)
	}
	summary, err := admissionMedia(raw.String(), admissionCaller("boundary-one", "boundary-two"))
	if err != nil || summary.Status != operator.Inconclusive || summary.CompletedCalls != 2 || summary.ExpectedCalls != 2 {
		t.Fatalf("product calls without operations: summary=%+v error=%v", summary, err)
	}
}

func TestAdmissionMediaRejectsInvalidProductEvidence(t *testing.T) {
	base := admissionCallLine(t, "boundary-one", false)
	for _, tc := range []struct{ name, old, replacement string }{
		{"malformed", `"Moves":null`, `"Moves":`},
		{"trailing-json", "\n", "{}\n"},
		{"wrong-pid-type", `"pid":42`, `"pid":"42"`},
		{"wrong-counter-type", `"AuthTag":0`, `"AuthTag":"0"`},
		{"wrong-moves-type", `"Moves":null`, `"Moves":{}`},
		{"wrong-armed-type", `"armed":null`, `"armed":{}`},
		{"duplicate-pid", `"pid":42`, `"pid":42,"pid":42`},
		{"duplicate-moves", `"Moves":null`, `"Moves":[],"Moves":null`},
		{"duplicate-armed", `"armed":null`, `"armed":[],"armed":null`},
		{"duplicate-extension", `"Moves":null`, `"Moves":null,"extension":{"a":1,"a":2}`},
		{"noncanonical-moves", `"Moves":null`, `"moves":null`},
		{"noncanonical-armed", `"armed":null`, `"Armed":null`},
		{"missing-moves", `"Moves":null,`, ""},
		{"missing-armed", `"armed":null,`, ""},
		{"missing-auth-tag", `"AuthTag":0,`, ""},
		{"missing-replay", `"Replay":0,`, ""},
		{"missing-other", `,"Other":0`, ""},
		{"missing-ice-restarts", `"ICERestarts":0,`, ""},
		{"missing-hangup", `"HungUpAt":1000000000,`, ""},
		{"null-hangup", `"HungUpAt":1000000000`, `"HungUpAt":null`},
		{"missing-started-at", `"StartedAt":"2026-11-21T00:00:00Z",`, ""},
		{"null-tracks", `"Tracks":[`, `"Tracks":null,"extension":[`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(base, tc.old, tc.replacement, 1)
			if raw == base {
				t.Fatal("fixture edit did not apply")
			}
			if _, err := admissionMedia(raw, admissionCaller("boundary-one")); err == nil {
				t.Fatal("accepted invalid product evidence")
			}
		})
	}
}

func TestAdmissionMediaRequiresOwnedCallers(t *testing.T) {
	for _, mode := range []string{"pid", "registration", "duplicate", "missing-call"} {
		t.Run(mode, func(t *testing.T) {
			b := admissionCaller("boundary-one")
			raw := admissionCallLine(t, "boundary-one", false)
			switch mode {
			case "pid":
				b.CallerPID++
			case "registration":
				b.Registration = nil
			case "duplicate":
				raw += raw
			case "missing-call":
				raw = `{"type":"call","pid":42,"session":"boundary-one"}`
			}
			_, err := admissionMedia(raw, b)
			if err == nil || !strings.Contains(err.Error(), "precondition-failed:caller-evidence") {
				t.Fatalf("lost caller precondition: %v", err)
			}
		})
	}
}

func TestAdmissionMediaPreservesKnownFailures(t *testing.T) {
	for _, mode := range []string{"auth-tag", "replay", "other", "ice-restart", "lost-move", "failed-move", "missing-hangup", "invalid-sibling"} {
		t.Run(mode, func(t *testing.T) {
			var m loadgen.Message
			if err := json.Unmarshal([]byte(admissionCallLine(t, "boundary-one", false)), &m); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "auth-tag", "missing-hangup", "invalid-sibling":
				m.Call.DecryptionFailures.AuthTag = 1
			case "replay":
				m.Call.DecryptionFailures.Replay = 1
			case "other":
				m.Call.DecryptionFailures.Other = 1
			case "ice-restart":
				m.Call.ICERestarts = 1
			case "lost-move":
				m.Call.Moves = []callharness.MoveReport{{Lost: true}}
			case "failed-move":
				m.Call.Moves = []callharness.MoveReport{{Measurement: callharness.EventMeasurement{Failed: true}}}
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing-hangup" {
				raw = bytes.Replace(raw, []byte(`"HungUpAt":1000000000,`), nil, 1)
			}
			if mode == "invalid-sibling" {
				raw = bytes.Replace(raw, []byte(`"HungUpAt":1000000000`), []byte(`"HungUpAt":"invalid"`), 1)
			}
			summary, err := admissionMedia(string(raw), admissionCaller("boundary-one"))
			if summary.Status != operator.Failure {
				t.Fatalf("known failure lost: summary=%+v error=%v", summary, err)
			}
			if (mode == "missing-hangup" || mode == "invalid-sibling") && err == nil {
				t.Fatal("failure concealed missing hangup evidence")
			}
		})
	}
}
