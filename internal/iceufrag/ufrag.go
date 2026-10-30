// Package iceufrag encodes the session and relay generation in an ICE ufrag.
package iceufrag

import (
	"strconv"
	"strings"
)

// Parse preserves bare generation-zero IDs. A suffix is canonical positive
// decimal, bounded by uint64. Existing IDs cannot contain '+' or ':'.
func Parse(ufrag string) (id string, generation uint64, ok bool) {
	id, suffix, hasGeneration := strings.Cut(ufrag, "+")
	if id == "" || strings.Contains(id, ":") {
		return "", 0, false
	}
	if !hasGeneration {
		return id, 0, true
	}
	if suffix == "" || suffix[0] == '0' {
		return "", 0, false
	}
	generation, err := strconv.ParseUint(suffix, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id, generation, true
}

// Format keeps generation zero byte-for-byte compatible with existing calls.
func Format(id string, generation uint64) string {
	if generation == 0 {
		return id
	}
	return id + "+" + strconv.FormatUint(generation, 10)
}
