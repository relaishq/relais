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
	pack := clockArtifactGZIP
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

func clockArtifactGZIP(raw []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(raw)
	_ = w.Close()
	return out.Bytes()
}

// Find a deterministic incompressible input using the real gzip encoder, rather
// than pinning a compressor-specific overhead or padding invalid gzip bytes.
func clockArtifactRawAtCompressedSize(t *testing.T, size int) []byte {
	t.Helper()
	noise := make([]byte, size)
	rng := rand.New(rand.NewPCG(17, 23))
	for i := range noise {
		noise[i] = byte(rng.Uint32())
	}
	n := size
	for range 8 {
		packed := clockArtifactGZIP(noise[:n])
		if len(packed) == size {
			return noise[:n]
		}
		n += size - len(packed)
		if n <= 0 || n > len(noise) {
			break
		}
	}
	t.Fatalf("cannot construct compressed-size fixture: %d", size)
	return nil
}

func TestClockBindingArtifactsExactWireLimit(t *testing.T) {
	// Pin the protocol's half-frame reservation independently of its implementation.
	const wantLimit = 128 << 10
	receipt := clockArtifactRawAtCompressedSize(t, wantLimit/4)
	for _, extra := range []int{0, 1} {
		evidence := clockArtifactRawAtCompressedSize(t, wantLimit*3/4+extra)
		for _, swap := range []bool{false, true} {
			r, e := receipt, evidence
			if swap {
				r, e = e, r
			}
			a, err := PackClockBindingArtifacts(r, e)
			if extra != 0 {
				if err == nil {
					t.Fatal("MUTATION: clock-artifacts-exact-wire-limit")
				}
				// Also enforce the combined limit on independently received bytes.
				a = ClockBindingArtifacts{clockArtifactGZIP(r), clockArtifactGZIP(e)}
				if len(a.ReceiptGZIP)+len(a.EvidenceGZIP) != wantLimit+1 {
					t.Fatal("fixture missed the combined limit+1")
				}
				if _, _, err := a.Unpack(); err == nil {
					t.Fatal("MUTATION: clock-artifacts-exact-wire-limit")
				}
				continue
			}
			if err != nil || len(a.ReceiptGZIP)+len(a.EvidenceGZIP) != wantLimit {
				t.Fatal("MUTATION: clock-artifacts-exact-wire-limit", err)
			}
			// Model the future Init envelope without adding a live protocol field.
			body := struct {
				Init
				ClockBinding ClockBindingArtifacts `json:"clockBinding"`
			}{Init: Init{Scenario: Scenario{SchemaVersion: 1, Gates: []Gate{Rollout}}, ArmedAtUnixMS: "1793930000000", EndUnixMS: "1793936300000", Capabilities: []Operation{HostExecute, HostReadBatch, CallerReady, CallerStart, CallerStop, MailboxRead}}, ClockBinding: a}
			frame := struct {
				Type    string `json:"type"`
				Session string `json:"session"`
				Init    any    `json:"init"`
			}{"init", "test-run", body}
			wire, err := json.Marshal(frame)
			if err != nil || len(wire)+1 > MaxFrameBytes {
				t.Fatal("MUTATION: clock-artifacts-exact-wire-limit", err, len(wire))
			}
			rawReceipt, rawEvidence, err := a.Unpack()
			if err != nil || !bytes.Equal(rawReceipt, r) || !bytes.Equal(rawEvidence, e) {
				t.Fatal("MUTATION: clock-artifacts-exact-wire-limit", err)
			}
		}
	}
}

func TestClockBindingArtifactsExactRawLimit(t *testing.T) {
	large, small := make([]byte, ClockArtifactLimit), []byte(`{}`)
	for _, swap := range []bool{false, true} {
		r, e := large, small
		if swap {
			r, e = e, r
		}
		a, err := PackClockBindingArtifacts(r, e)
		if err != nil {
			t.Fatal("MUTATION: clock-artifacts-exact-raw-limit", err)
		}
		rawReceipt, rawEvidence, err := a.Unpack()
		if err != nil || !bytes.Equal(rawReceipt, r) || !bytes.Equal(rawEvidence, e) {
			t.Fatal("MUTATION: clock-artifacts-exact-raw-limit", err)
		}
	}
}
