package operatorrehearsal

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestUnboundMainBytes(t *testing.T) {
	var b bytes.Buffer
	err := passingReport().Write(&b)
	raw := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(raw))
	if got != "14d2add9a449afed61b16af44b737c15f600a9cde7251bc258858866582e695a" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
