package redistestcluster

import (
	"strings"
	"testing"
)

// Slot implements Redis Cluster's CRC16/XMODEM slot rule, including empty tags.
func Slot(key string) uint16 {
	if start := strings.IndexByte(key, '{'); start >= 0 {
		if end := strings.IndexByte(key[start+1:], '}'); end > 0 {
			key = key[start+1 : start+1+end]
		}
	}
	var crc uint16
	for i := range len(key) {
		crc ^= uint16(key[i]) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc % 16384
}

// SameSlot fails if a script or atomic command would cross Redis Cluster slots.
func SameSlot(t *testing.T, keys ...string) {
	t.Helper()
	if len(keys) == 0 {
		t.Fatal("a routed operation must have at least one key")
	}
	for _, key := range keys[1:] {
		if Slot(key) != Slot(keys[0]) {
			t.Fatalf("CROSSSLOT: %q (%d) and %q (%d)", keys[0], Slot(keys[0]), key, Slot(key))
		}
	}
}
