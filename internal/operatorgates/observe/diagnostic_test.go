package observe

import (
	"errors"
	"reflect"
	"testing"

	"github.com/relaishq/relais/pkg/callharness"
)

func TestEvidenceIssuesNeverPublishValuesOrMapKeys(t *testing.T) {
	for _, row := range []struct {
		err          error
		path, reason string
	}{
		{errors.New("required evidence $.Moves[0].distributed.gaps missing or null"), "$.Moves[0].distributed.gaps", "missing-or-null"},
		{errors.New("required evidence $.Moves[private-map-secret].Kind missing or null"), "$.Moves[*].Kind", "missing-or-null"},
		{errors.New("required evidence $.privateSecret missing or null"), "$", "missing-or-null"},
		{errors.New("Moves: Distributed: Gaps: EventGap: Lower: negative or out-of-range number"), "$.Moves.Distributed.Gaps.EventGap.Lower", "negative-or-out-of-range-number"},
		{errors.New("server said credential-private-string"), "$", "invalid-evidence"},
		{errors.New("required evidence "), "$", "invalid-evidence"},
	} {
		got := DescribeEvidence(row.err).Public(reflect.TypeFor[callharness.Report]())
		if got.Path != row.path || got.Reason != row.reason {
			t.Fatalf("got %+v want %s %s", got, row.path, row.reason)
		}
	}
	hostile := EvidenceIssue{Path: "$.Moves[0].Kind/private", Reason: "secret-value"}.Public(reflect.TypeFor[callharness.Report]())
	if hostile.Path != "$" || hostile.Reason != "invalid-evidence" {
		t.Fatal(hostile)
	}
}
