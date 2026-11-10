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
	if got != "b5f5b4b4b58d7af9d7a3b804d822a2b0b085bd2da098c93f16de209e44f48736" {
		t.Fatal("MUTATION: unbound-main-bytes", got)
	}
}
