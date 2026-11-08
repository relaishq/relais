package operator

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

// ClockArtifactLimit matches the selector and binding module's per-artifact
// limit. Compression makes admission evidence fit without enlarging wire frames.
const ClockArtifactLimit = 8 << 20

// Leave room for base64 expansion, scenario identity and the enclosing frame.
const clockArtifactsWireLimit = MaxFrameBytes / 2

var errClockArtifacts = errors.New("invalid-clock-binding-artifacts")

// ClockBindingArtifacts transports the original receipt and selection evidence.
// This preparation API is not accepted by Init yet. Unpack only checks transport;
// BindReceipt must recompute both digests and validate the returned artifact bytes.
type ClockBindingArtifacts struct {
	ReceiptGZIP  []byte `json:"receiptGzip"`
	EvidenceGZIP []byte `json:"evidenceGzip"`
}

func PackClockBindingArtifacts(receipt, evidence []byte) (ClockBindingArtifacts, error) {
	if len(receipt) == 0 || len(evidence) == 0 || len(receipt) > ClockArtifactLimit || len(evidence) > ClockArtifactLimit {
		return ClockBindingArtifacts{}, errClockArtifacts
	}
	pack := func(raw []byte) []byte {
		var out bytes.Buffer
		w := gzip.NewWriter(&out)
		// A bytes.Buffer cannot fail writes or closure.
		_, _ = w.Write(raw)
		_ = w.Close()
		return out.Bytes()
	}
	a := ClockBindingArtifacts{ReceiptGZIP: pack(receipt), EvidenceGZIP: pack(evidence)}
	if !a.fitsWire() {
		return ClockBindingArtifacts{}, errClockArtifacts
	}
	return a, nil
}

func (a ClockBindingArtifacts) fitsWire() bool {
	return len(a.ReceiptGZIP) > 0 && len(a.EvidenceGZIP) > 0 && len(a.ReceiptGZIP) <= clockArtifactsWireLimit && len(a.EvidenceGZIP) <= clockArtifactsWireLimit-len(a.ReceiptGZIP)
}

func (a ClockBindingArtifacts) Unpack() (receipt, evidence []byte, err error) {
	if !a.fitsWire() {
		return nil, nil, errClockArtifacts
	}
	receipt, err = unpackClockArtifact(a.ReceiptGZIP)
	if err != nil {
		return nil, nil, err
	}
	evidence, err = unpackClockArtifact(a.EvidenceGZIP)
	if err != nil {
		return nil, nil, err
	}
	return receipt, evidence, nil
}

func unpackClockArtifact(packed []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, errClockArtifacts
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, ClockArtifactLimit+1))
	if err != nil || len(raw) == 0 || len(raw) > ClockArtifactLimit {
		return nil, errClockArtifacts
	}
	return raw, nil
}
