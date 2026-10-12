package callharness

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/prf"
	"github.com/pion/dtls/v3/pkg/protocol/extension"
	"github.com/pion/dtls/v3/pkg/protocol/handshake"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

const maxCryptoFailures = 128

// SRTPDiagnosticReport is a shadow receiver's evidence, not the caller verdict.
// Only packet metadata is retained. Keys and plaintext never enter the report.
type SRTPDiagnosticReport struct {
	Ready          bool
	Unavailable    string `json:",omitempty"`
	Packets        uint64
	Skipped        uint64
	Failures       DecryptionFailures
	Samples        []SRTPFailureSample
	SamplesOmitted uint64
}

// SRTPFailureSample identifies ciphertext without exposing its payload. At uses
// the same origin as moves. AuthenticatedIndex is populated only when a fresh
// context verifies an auth failure using an adjacent ROC (rollover counter).
// This probe does not deliver the packet or update the shadow receiver.
type SRTPFailureSample struct {
	At                 time.Duration
	Protocol           string
	SSRC               uint32
	Sequence           uint16 `json:",omitempty"`
	Timestamp          uint32 `json:",omitempty"`
	PayloadType        uint8  `json:",omitempty"`
	EstimatedIndex     uint64
	PreviousIndex      uint64
	PreviousIndexKnown bool
	AuthenticatedIndex *uint64 `json:",omitempty"`
	CiphertextSHA256   string
	Bytes              int
	Error              string
}

type cryptoObserver struct {
	mu           sync.Mutex
	start        time.Time
	serverRandom [32]byte
	haveHello    bool
	profile      srtp.ProtectionProfile
	hash         prf.HashFunc
	keys         srtp.SessionKeys
	receiver     *srtp.Context
	highest      map[uint32]uint64
	stats        SRTPDiagnosticReport
}

func newCryptoObserver(start time.Time) *cryptoObserver {
	return &cryptoObserver{start: start, highest: make(map[uint32]uint64)}
}

// Write is Pion's key-log callback. Consume one TLS 1.2 secret directly in
// memory, derive the SRTP keys with Pion's PRF, then discard the secret. The
// diagnostic only supports the ordinary client role used by this harness.
func (o *cryptoObserver) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fields := strings.Fields(string(data))
	if len(fields) != 3 || fields[0] != "CLIENT_RANDOM" || !o.haveHello || o.hash == nil {
		o.stats.Unavailable = "DTLS client secret or supported ServerHello unavailable"
		return len(data), nil
	}
	random, err := hex.DecodeString(fields[1])
	if err != nil || len(random) != 32 {
		o.stats.Unavailable = "invalid DTLS client random"
		return len(data), nil
	}
	secret, err := hex.DecodeString(fields[2])
	if err != nil {
		o.stats.Unavailable = "invalid DTLS secret"
		return len(data), nil
	}
	seed := append([]byte("EXTRACTOR-dtls_srtp"), random...)
	seed = append(seed, o.serverRandom[:]...)
	cfg := srtp.Config{Profile: o.profile}
	err = cfg.ExtractSessionKeysFromDTLS(cryptoExporter{secret: secret, seed: seed, hash: o.hash}, true)
	clear(secret)
	if err == nil {
		// Match Pion SessionSRTP/SessionSRTCP's default 64-packet replay windows.
		o.receiver, err = srtp.CreateContext(cfg.Keys.RemoteMasterKey, cfg.Keys.RemoteMasterSalt, o.profile,
			srtp.SRTPReplayProtection(64), srtp.SRTCPReplayProtection(64))
	}
	if err != nil {
		o.stats.Unavailable = "cannot initialize shadow SRTP receiver"
		return len(data), nil
	}
	o.keys = cfg.Keys
	o.stats.Ready = true
	o.stats.Unavailable = ""
	return len(data), nil
}

type cryptoExporter struct {
	secret, seed []byte
	hash         prf.HashFunc
}

func (e cryptoExporter) ExportKeyingMaterial(label string, context []byte, length int) ([]byte, error) {
	if label != "EXTRACTOR-dtls_srtp" || len(context) != 0 {
		return nil, errors.New("unsupported diagnostic DTLS exporter")
	}
	return prf.PHash(e.secret, e.seed, length, e.hash)
}

// hello reads only plaintext epoch-zero ServerHello records. Fragmented or
// unsupported handshakes leave diagnostics unavailable, without affecting DTLS.
func (o *cryptoObserver) hello(raw []byte) {
	for len(raw) >= 13 {
		size := 13 + int(binary.BigEndian.Uint16(raw[11:13]))
		if size > len(raw) {
			return
		}
		record := raw[:size]
		raw = raw[size:]
		if record[0] != 22 || binary.BigEndian.Uint16(record[3:5]) != 0 {
			continue
		}
		body := record[13:]
		for len(body) >= handshake.HeaderLength {
			var header handshake.Header
			if header.Unmarshal(body) != nil {
				break
			}
			size := handshake.HeaderLength + int(header.FragmentLength)
			if size > len(body) {
				break
			}
			fragment := body[:size]
			body = body[size:]
			if header.Type != handshake.TypeServerHello || header.FragmentOffset != 0 || header.FragmentLength != header.Length {
				continue
			}
			var hello handshake.MessageServerHello
			if hello.Unmarshal(fragment[handshake.HeaderLength:]) != nil || hello.CipherSuiteID == nil {
				continue
			}
			switch dtls.CipherSuiteID(*hello.CipherSuiteID) {
			case dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, dtls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				dtls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, dtls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256:
				o.hash = sha256.New
			case dtls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, dtls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:
				o.hash = sha512.New384
			default:
				o.stats.Unavailable = "unsupported diagnostic DTLS cipher suite"
				return
			}
			for _, ext := range hello.Extensions {
				if use, ok := ext.(*extension.UseSRTP); ok && len(use.ProtectionProfiles) == 1 && len(use.MasterKeyIdentifier) == 0 {
					o.profile = srtp.ProtectionProfile(use.ProtectionProfiles[0])
					o.serverRandom = hello.Random.MarshalFixed()
					o.haveHello = true
				}
			}
		}
	}
}

func (o *cryptoObserver) observe(raw []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(raw) == 0 {
		return
	}
	if raw[0] >= 20 && raw[0] <= 63 {
		if !o.stats.Ready {
			o.hello(raw)
		}
		return
	}
	if len(raw) < 8 || raw[0]>>6 != 2 {
		return
	}
	if !o.stats.Ready {
		o.stats.Skipped++
		return
	}
	o.stats.Packets++
	sample := SRTPFailureSample{At: time.Since(o.start), Bytes: len(raw)}
	var err error
	if raw[1] >= 192 && raw[1] <= 223 {
		sample.Protocol = "SRTCP"
		sample.SSRC = binary.BigEndian.Uint32(raw[4:8])
		tag, _ := o.profile.AuthTagRTCPLen()
		if len(raw) >= 8+4+tag {
			sample.EstimatedIndex = uint64(binary.BigEndian.Uint32(raw[len(raw)-tag-4:]) & 0x7fffffff)
		}
		_, err = o.receiver.DecryptRTCP(nil, raw, nil)
	} else {
		sample.Protocol = "SRTP"
		var header rtp.Header
		if _, err = header.Unmarshal(raw); err == nil {
			sample.SSRC, sample.Sequence, sample.Timestamp, sample.PayloadType = header.SSRC, header.SequenceNumber, header.Timestamp, header.PayloadType
			sample.PreviousIndex, sample.PreviousIndexKnown = o.highest[header.SSRC]
			sample.EstimatedIndex = diagnosticRTPIndex(sample.PreviousIndex, header.SequenceNumber)
			_, err = o.receiver.DecryptRTP(nil, raw, &header)
			if err == nil {
				// Pion's ROC is the context's highest, not this packet's. A late
				// packet from before a wrap must not raise the index by 2^16.
				o.highest[header.SSRC] = max(o.highest[header.SSRC], sample.EstimatedIndex)
			} else if errors.Is(err, srtp.ErrFailedToVerifyAuthTag) && len(o.stats.Samples) < maxCryptoFailures {
				sample.AuthenticatedIndex = o.authenticateAdjacentROC(raw, sample)
			}
		}
	}
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, srtp.ErrFailedToVerifyAuthTag):
		o.stats.Failures.AuthTag++
	case strings.Contains(err.Error(), "duplicated"):
		o.stats.Failures.Replay++
	default:
		o.stats.Failures.Other++
	}
	if len(o.stats.Samples) == maxCryptoFailures {
		o.stats.SamplesOmitted++
		return
	}
	sum := sha256.Sum256(raw)
	sample.CiphertextSHA256 = hex.EncodeToString(sum[:])
	sample.Error = err.Error()
	o.stats.Samples = append(o.stats.Samples, sample)
}

// Match the pinned Pion context's ROC-zero underflow guard as well as RFC 3711.
func diagnosticRTPIndex(highest uint64, seq uint16) uint64 {
	roc, last := uint32(highest>>16), int32(highest&0xffff) //nolint:gosec // 48-bit SRTP index
	if highest > 1<<15 {
		if last < 1<<15 && int32(seq)-last > 1<<15 {
			roc--
		} else if last >= 1<<15 && last-(1<<15) > int32(seq) {
			roc++
		}
	}
	return uint64(roc)<<16 | uint64(seq)
}

func (o *cryptoObserver) authenticateAdjacentROC(raw []byte, sample SRTPFailureSample) *uint64 {
	roc := uint32(sample.EstimatedIndex >> 16) //nolint:gosec // 48-bit SRTP index
	for _, candidate := range []uint32{roc - 1, roc + 1} {
		probe, err := srtp.CreateContext(o.keys.RemoteMasterKey, o.keys.RemoteMasterSalt, o.profile)
		if err != nil {
			continue
		}
		probe.SetROC(sample.SSRC, candidate)
		if _, err := probe.DecryptRTP(nil, raw, nil); err == nil {
			index := uint64(candidate)<<16 | uint64(sample.Sequence)
			return &index
		}
	}
	return nil
}

func (o *cryptoObserver) report() *SRTPDiagnosticReport {
	o.mu.Lock()
	defer o.mu.Unlock()
	report := o.stats
	report.Samples = slices.Clone(report.Samples)
	if !report.Ready && report.Unavailable == "" {
		report.Unavailable = "DTLS diagnostic keys unavailable"
	}
	return &report
}
