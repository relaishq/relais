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
	// The passing fixture contains 20 moves, 16 takeovers, 36 events, and
	// eight host-loss call gaps. #540 adds the required relayLoss projection.
	if got != "6e4daaf1594eea3c13621f3b8865b20f943b42ef58655fbeb8f3f479507e345d" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
