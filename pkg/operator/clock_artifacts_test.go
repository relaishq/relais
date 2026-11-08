package operator

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"math/rand/v2"
	"testing"
)

func TestClockBindingArtifactsRoundTrip(t *testing.T) {
	receipt := []byte(`{"version":1,"runId":"test-run"}`)
	// A normal admission artifact already exceeds the uncompressed frame limit.
	evidence := bytes.Repeat([]byte(`{"maxErrorDecimal":"1.234e-05"},`), 11000)
	if len(evidence) <= MaxFrameBytes {
		t.Fatal("fixture must exceed the frame limit")
	}
	a, err := PackClockBindingArtifacts(receipt, evidence)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(a)
	if err != nil || len(wire) >= MaxFrameBytes {
		t.Fatalf("wire budget: bytes=%d err=%v", len(wire), err)
	}
	var decoded ClockBindingArtifacts
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	r, e, err := decoded.Unpack()
	if err != nil || !bytes.Equal(r, receipt) || !bytes.Equal(e, evidence) {
		t.Fatalf("MUTATION: clock-artifacts-roundtrip: %v", err)
	}
	again, err := PackClockBindingArtifacts(receipt, evidence)
	if err != nil || !bytes.Equal(a.ReceiptGZIP, again.ReceiptGZIP) || !bytes.Equal(a.EvidenceGZIP, again.EvidenceGZIP) {
		t.Fatal("artifact packing is not deterministic")
	}
	// The codec preserves arbitrary source bytes. Semantic validation and digest
	// recomputation belong to BindReceipt, never a supplied digest on the wire.
	if sha256.Sum256(e) != sha256.Sum256(evidence) {
		t.Fatal("evidence commitment changed")
	}
}

func TestClockBindingArtifactsLimits(t *testing.T) {
	pack := func(raw []byte) []byte {
		var out bytes.Buffer
		w := gzip.NewWriter(&out)
		_, _ = w.Write(raw)
		_ = w.Close()
		return out.Bytes()
	}
	t.Run("raw", func(t *testing.T) {
		for _, oversized := range []bool{false, true} {
			for _, receiptFirst := range []bool{false, true} {
				raw := []byte{}
				if oversized {
					raw = make([]byte, ClockArtifactLimit+1)
				}
				r, e := raw, []byte(`{}`)
				if !receiptFirst {
					r, e = e, r
				}
				if _, err := PackClockBindingArtifacts(r, e); err == nil {
					t.Fatal("MUTATION: clock-artifacts-raw-limit")
				}
			}
		}
	})
	t.Run("wire", func(t *testing.T) {
		noise := make([]byte, MaxFrameBytes)
		rng := rand.New(rand.NewPCG(17, 23))
		for i := range noise {
			noise[i] = byte(rng.Uint32())
		}
		if _, err := PackClockBindingArtifacts([]byte(`{}`), noise); err == nil {
			t.Fatal("MUTATION: clock-artifacts-wire-limit")
		}
		large := pack(noise)
		for _, a := range []ClockBindingArtifacts{{large, pack([]byte(`{}`))}, {pack([]byte(`{}`)), large}, {nil, nil}} {
			if _, _, err := a.Unpack(); err == nil {
				t.Fatal("MUTATION: clock-artifacts-wire-limit")
			}
		}
	})
	t.Run("decoded", func(t *testing.T) {
		for _, n := range []int{0, ClockArtifactLimit, ClockArtifactLimit + 1} {
			packed := pack(make([]byte, n))
			for _, receiptFirst := range []bool{false, true} {
				a := ClockBindingArtifacts{packed, pack([]byte(`{}`))}
				if !receiptFirst {
					a.ReceiptGZIP, a.EvidenceGZIP = a.EvidenceGZIP, a.ReceiptGZIP
				}
				_, _, err := a.Unpack()
				if (err == nil) != (n == ClockArtifactLimit) {
					t.Fatal("MUTATION: clock-artifacts-decoded-limit")
				}
			}
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		valid := pack([]byte(`{}`))
		badCRC := bytes.Clone(valid)
		badCRC[len(badCRC)-8] ^= 1
		for _, bad := range [][]byte{[]byte("not gzip"), valid[:len(valid)-1], badCRC} {
			for _, receiptFirst := range []bool{false, true} {
				a := ClockBindingArtifacts{bad, valid}
				if !receiptFirst {
					a.ReceiptGZIP, a.EvidenceGZIP = a.EvidenceGZIP, a.ReceiptGZIP
				}
				if _, _, err := a.Unpack(); err == nil {
					t.Fatal("MUTATION: clock-artifacts-corrupt")
				}
			}
		}
	})
}
