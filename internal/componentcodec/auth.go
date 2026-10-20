// Package componentcodec defines the frozen v10 checkpoint authentication format.
package componentcodec

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Five references: setup, work, history, app root, whole checkpoint. Metadata
// is stored once as m and authenticated as associated data, never in payloads.
// Store-generated commit time is t, not client-predicted associated data.
type Reference struct {
	Digest   [32]byte
	KeyID    uint8
	Revision uint64
}
type Metadata struct {
	Format          uint8
	References      [5]Reference
	OwnerEpoch      uint64
	StateVersion    uint32
	Banks           [3]uint8
	Owner           string
	SetupGeneration uint64
}

const (
	MetadataMaxBytes = 295 // owner length is at most 64 UTF-8 bytes
	CommitTimeBytes  = 8
	FieldNameBytes   = 3 // b/m/t
	ProtocolReserve  = 78
	ReferenceReserve = MetadataMaxBytes + CommitTimeBytes + FieldNameBytes + ProtocolReserve // 384
	CipherOverhead   = 2 + 12 + 16                                                           // header + random nonce + GCM tag
)

func (m Metadata) Marshal() ([]byte, error) {
	if m.Format != 10 || len(m.Owner) > 64 || m.Owner == "" {
		return nil, fmt.Errorf("%w: component metadata format/owner", ErrInvalid)
	}
	for _, bank := range m.Banks {
		if bank > 1 {
			return nil, fmt.Errorf("%w: component bank", ErrInvalid)
		}
	}
	var b bytes.Buffer
	b.WriteByte(m.Format)
	for _, ref := range m.References {
		b.Write(ref.Digest[:])
		b.WriteByte(ref.KeyID)
		_ = binary.Write(&b, binary.BigEndian, ref.Revision)
	}
	_ = binary.Write(&b, binary.BigEndian, m.OwnerEpoch)
	_ = binary.Write(&b, binary.BigEndian, m.StateVersion)
	b.Write(m.Banks[:])
	_ = binary.Write(&b, binary.BigEndian, uint16(len(m.Owner)))
	b.WriteString(m.Owner)
	_ = binary.Write(&b, binary.BigEndian, m.SetupGeneration)
	return b.Bytes(), nil
}
func DecodeMetadata(data []byte) (Metadata, error) {
	var m Metadata
	if len(data) > MetadataMaxBytes {
		return m, fmt.Errorf("%w: metadata size", ErrInvalid)
	}
	r := bytes.NewReader(data)
	if err := binary.Read(r, binary.BigEndian, &m.Format); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	for i := range m.References {
		if _, err := io.ReadFull(r, m.References[i].Digest[:]); err != nil {
			return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
		}
		if err := binary.Read(r, binary.BigEndian, &m.References[i].KeyID); err != nil {
			return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
		}
		if err := binary.Read(r, binary.BigEndian, &m.References[i].Revision); err != nil {
			return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
		}
	}
	if err := binary.Read(r, binary.BigEndian, &m.OwnerEpoch); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	if err := binary.Read(r, binary.BigEndian, &m.StateVersion); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	if _, err := io.ReadFull(r, m.Banks[:]); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	if int(n) > r.Len() || n > 64 {
		return m, fmt.Errorf("%w: metadata owner", ErrInvalid)
	}
	owner := make([]byte, n)
	if _, err := io.ReadFull(r, owner); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	m.Owner = string(owner)
	if err := binary.Read(r, binary.BigEndian, &m.SetupGeneration); err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	canonical, err := m.Marshal()
	if err != nil {
		return m, fmt.Errorf("%w: component metadata: %w", ErrInvalid, err)
	}
	if !bytes.Equal(canonical, data) {
		return m, fmt.Errorf("%w: noncanonical metadata", ErrInvalid)
	}
	return m, nil
}

func Key(master []byte, sessionID string, kind byte, purpose string) ([]byte, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("%w: component key length", ErrInvalid)
	}
	if sessionID == "" || len(sessionID) > DecodeLimit {
		return nil, fmt.Errorf("%w: session identity", ErrInvalid)
	}
	// Length prefixes keep session IDs unambiguous even when they contain
	// separators or share prefixes. Both AES and HMAC keys are per session.
	info := binary.BigEndian.AppendUint32([]byte("relais/session/v10/"), uint32(len(sessionID)))
	info = append(info, sessionID...)
	info = append(info, kind)
	info = binary.BigEndian.AppendUint32(info, uint32(len(purpose)))
	info = append(info, purpose...)
	return hkdf.Key(sha256.New, master, nil, string(info), 32)
}
func Digest(master []byte, sessionID string, kind byte, data []byte) ([32]byte, error) {
	var digest [32]byte
	key, err := Key(master, sessionID, kind, "digest")
	if err != nil {
		return digest, err
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	copy(digest[:], h.Sum(nil))
	return digest, nil
}
func RootDigest(master []byte, sessionID string, version uint32, work, history Reference) ([32]byte, error) {
	var b bytes.Buffer
	// Canonical length-prefixed tuple; key IDs accompany retained digests.
	for _, part := range [][]byte{binary.BigEndian.AppendUint32(nil, version), ReferenceBytes(work), ReferenceBytes(history)} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(part)))
		b.Write(part)
	}
	return Digest(master, sessionID, Root, b.Bytes())
}
func ReferenceBytes(ref Reference) []byte {
	b := []byte{ref.KeyID}
	b = binary.BigEndian.AppendUint64(b, ref.Revision)
	return append(b, ref.Digest[:]...)
}
func WholeDigest(master []byte, sessionID string, metadata Metadata, transport []byte) ([32]byte, error) {
	// The whole-checkpoint digest excludes only itself to avoid a circular hash.
	metadata.References[4].Digest = [32]byte{}
	m, err := metadata.Marshal()
	if err != nil {
		return [32]byte{}, err
	}
	return Digest(master, sessionID, Checkpoint, append(m, transport...))
}

func AAD(sessionID string, kind byte, m Metadata, header []byte) ([]byte, error) {
	if sessionID == "" || len(sessionID) > DecodeLimit {
		return nil, fmt.Errorf("%w: session identity", ErrInvalid)
	}
	var b bytes.Buffer
	b.WriteString("relais/session/v10/aad")
	_ = binary.Write(&b, binary.BigEndian, uint32(len(sessionID)))
	b.WriteString(sessionID)
	b.WriteByte(kind)
	_ = binary.Write(&b, binary.BigEndian, m.SetupGeneration)
	b.Write(header)
	if kind == Transport {
		packed, err := m.Marshal()
		if err != nil {
			return nil, err
		}
		b.Write(packed)
	} else {
		index := int(kind) - 1
		if kind == Work {
			index = 1
		}
		if kind == History {
			index = 2
		}
		if index < 0 || index > 2 {
			return nil, fmt.Errorf("%w: component kind", ErrInvalid)
		}
		b.WriteByte(m.Banks[index])
		b.Write(ReferenceBytes(m.References[index]))
		if kind != Setup {
			_ = binary.Write(&b, binary.BigEndian, m.StateVersion)
		}
	}
	return b.Bytes(), nil
}
func AEAD(master []byte, sessionID string, kind byte) (cipher.AEAD, error) {
	key, err := Key(master, sessionID, kind, "encryption")
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func Seal(master []byte, keyID byte, sessionID string, kind byte, m Metadata, payload []byte) ([]byte, error) {
	aead, err := AEAD(master, sessionID, kind)
	if err != nil {
		return nil, err
	}
	header := []byte{1, keyID}
	aad, err := AAD(sessionID, kind, m, header)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	prefix := append(header, nonce...)
	return aead.Seal(prefix, nonce, payload, aad), nil
}
func Open(master []byte, keyID byte, sessionID string, kind byte, m Metadata, sealed []byte) ([]byte, error) {
	if len(sealed) < CipherOverhead || sealed[0] != 1 || sealed[1] != keyID {
		return nil, fmt.Errorf("%w: encryption header", ErrInvalid)
	}
	aead, err := AEAD(master, sessionID, kind)
	if err != nil {
		return nil, err
	}
	aad, err := AAD(sessionID, kind, m, sealed[:2])
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, sealed[2:14], sealed[14:], aad)
	if err != nil {
		return nil, fmt.Errorf("%w: component authentication: %w", ErrInvalid, err)
	}
	return plain, nil
}

func SealAll(master []byte, keyID byte, sessionID string, m Metadata, c Components) (Components, Metadata, error) {
	var sealed Components
	for i, part := range [][]byte{c.Setup, c.Work, c.History} {
		kind := []byte{Setup, Work, History}[i]
		digest, err := Digest(master, sessionID, kind, part)
		if err != nil {
			return sealed, m, err
		}
		m.References[i].Digest, m.References[i].KeyID = digest, keyID
	}
	root, err := RootDigest(master, sessionID, m.StateVersion, m.References[1], m.References[2])
	if err != nil {
		return sealed, m, err
	}
	m.References[3].Digest, m.References[3].KeyID = root, keyID
	m.References[4].KeyID = keyID
	whole, err := WholeDigest(master, sessionID, m, c.Transport)
	if err != nil {
		return sealed, m, err
	}
	m.References[4].Digest = whole
	for _, part := range []struct {
		kind  byte
		plain []byte
		out   *[]byte
	}{{Setup, c.Setup, &sealed.Setup}, {Transport, c.Transport, &sealed.Transport}, {Work, c.Work, &sealed.Work}, {History, c.History, &sealed.History}} {
		*part.out, err = Seal(master, keyID, sessionID, part.kind, m, part.plain)
		if err != nil {
			return Components{}, m, err
		}
	}
	return sealed, m, nil
}

func OpenAll(keys map[uint8][]byte, sessionID string, m Metadata, sealed Components) (Components, error) {
	var c Components
	for _, part := range []struct {
		kind   byte
		index  int
		sealed []byte
		out    *[]byte
	}{{Setup, 0, sealed.Setup, &c.Setup}, {Transport, 4, sealed.Transport, &c.Transport}, {Work, 1, sealed.Work, &c.Work}, {History, 2, sealed.History, &c.History}} {
		ref := m.References[part.index]
		plain, err := Open(keys[ref.KeyID], ref.KeyID, sessionID, part.kind, m, part.sealed)
		if err != nil {
			return Components{}, err
		}
		var digest [32]byte
		if part.kind == Transport {
			digest, err = WholeDigest(keys[ref.KeyID], sessionID, m, plain)
		} else {
			digest, err = Digest(keys[ref.KeyID], sessionID, part.kind, plain)
		}
		if err != nil {
			return Components{}, err
		}
		if !hmac.Equal(digest[:], ref.Digest[:]) {
			return Components{}, fmt.Errorf("%w: component digest", ErrInvalid)
		}
		*part.out = plain
	}
	root, err := RootDigest(keys[m.References[3].KeyID], sessionID, m.StateVersion, m.References[1], m.References[2])
	if err != nil {
		return Components{}, err
	}
	if !hmac.Equal(root[:], m.References[3].Digest[:]) {
		return Components{}, fmt.Errorf("%w: app root digest", ErrInvalid)
	}
	return c, nil
}

const (
	Setup       byte = 1
	Transport   byte = 2
	Work        byte = 3
	History     byte = 4
	Root        byte = 5
	Checkpoint  byte = 6
	DecodeLimit      = 128 << 10
)

var ErrInvalid = errors.New("invalid v10 component")

type Components struct{ Setup, Transport, Work, History []byte }
