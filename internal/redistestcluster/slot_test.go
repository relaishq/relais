package redistestcluster

import "testing"

func TestSlotCRC16(t *testing.T) {
	for key, want := range map[string]uint16{"123456789": 12739, "foo{123456789}bar": 12739, "{user1000}.following": 3443, "{user1000}.followers": 3443} {
		if got := Slot(key); got != want {
			t.Fatalf("Slot(%q)=%d, want %d", key, got, want)
		}
	}
	if Slot("a{}b{tag}") == Slot("tag") {
		t.Fatal("empty first tag must hash the full key")
	}
}
