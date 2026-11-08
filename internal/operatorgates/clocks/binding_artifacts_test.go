package clocks

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/relaishq/relais/pkg/operator"
)

func TestReceiptBindingArtifactTransport(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		in := bindingFixture(t)
		evidence := in.EvidenceJSON
		if tampered {
			evidence = bytes.Replace(evidence, []byte(`"maxErrorDecimal":"1.234e-06"`), []byte(`"maxErrorDecimal":"1.235e-06"`), 1)
			if bytes.Equal(evidence, in.EvidenceJSON) {
				t.Fatal("fixture mutation missed")
			}
		}
		artifacts, err := operator.PackClockBindingArtifacts(in.ReceiptJSON, evidence)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(artifacts)
		if err != nil || len(wire) >= operator.MaxFrameBytes {
			t.Fatalf("wire frame: %v", err)
		}
		var received operator.ClockBindingArtifacts
		if err = json.Unmarshal(wire, &received); err != nil {
			t.Fatal(err)
		}
		in.ReceiptJSON, in.EvidenceJSON, err = received.Unpack()
		if err != nil {
			t.Fatal(err)
		}
		b, err := BindReceipt(in)
		if !tampered {
			if err != nil || b.ReceiptSHA256 != in.ReceiptSHA256 || b.EvidenceSHA256 != in.EvidenceSHA256 {
				t.Fatalf("transported binding: %+v %v", b, err)
			}
		} else {
			var refusal *BindingError
			if !errors.As(err, &refusal) || refusal.Cause != "clock-binding-digest-differ" {
				t.Fatalf("transported bytes bypassed evidence commitment: %v", err)
			}
		}
	}
}
