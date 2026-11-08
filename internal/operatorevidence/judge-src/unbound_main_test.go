package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestUnboundMainBytes(t *testing.T) {
	input := coveragePoolFixture(t, "covered")
	raw, err := json.Marshal(pooledPreviewMedia(&input))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(raw))
	if got != "98459fa86d8660f322cdcc94fb3f21171394aa12823ba68f4b99feffa781dbb3" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
