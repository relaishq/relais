package sessionstore

import (
	"bytes"
	"crypto/sha1"
)

// Redis provides SHA-1 for a reconstruction checksum. This is not an
// authenticator: the full metadata remains bound by HMAC-SHA256 and AEAD.
// The checksum covers the original metadata, never the encoded delta.
func encodeMetadataDelta(expected, metadata []byte) []byte {
	checksum := sha1.Sum(metadata)
	out := append([]byte(nil), checksum[:]...)
	if len(expected) < 232 || len(metadata) < 232 || !bytes.Equal(expected[:1], metadata[:1]) || !bytes.Equal(expected[206:218], metadata[206:218]) || !bytes.Equal(expected[221:], metadata[221:]) {
		return append(out, metadata...)
	}
	var mask byte
	for i := 0; i < 5; i++ {
		if !bytes.Equal(expected[1+i*41:42+i*41], metadata[1+i*41:42+i*41]) {
			mask |= 1 << i
		}
	}
	if mask&7 == 0 && !bytes.Equal(expected[218:221], metadata[218:221]) {
		return append(out, metadata...)
	}
	out = append(out, 0x80|mask)
	for i := 0; i < 5; i++ {
		if mask&(1<<i) != 0 {
			out = append(out, metadata[1+i*41:42+i*41]...)
		}
	}
	if mask&7 != 0 {
		out = append(out, metadata[218:221]...)
	}
	return out
}
