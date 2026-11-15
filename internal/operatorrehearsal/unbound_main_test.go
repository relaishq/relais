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
	// The passing fixture contains 20 moves, 16 takeovers and 36 events.
	if got != "331c52902955ef27a4af6a71026807af41463b71966260b9e6dc2f7c3ea95aca" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
