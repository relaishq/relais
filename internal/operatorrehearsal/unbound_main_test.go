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
	if got != "0563aa7c7ed3a797b42f3ca03a36a678a4fc0a81301129254919da2391b2ba37" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
