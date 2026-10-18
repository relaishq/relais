package mediaworker

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// Five references: setup, work, history, app root, whole checkpoint. Metadata
// is stored once as m and authenticated as associated data, never in payloads.
// Store-generated commit time is t, not client-predicted associated data.
type componentReference struct {
	Digest   [32]byte
	KeyID    uint8
	Revision uint64
}
type componentMetadata struct {
	Format          uint8
	References      [5]componentReference
	OwnerEpoch      uint64
	StateVersion    uint32
	Banks           [3]uint8
	Owner           string
	SetupGeneration uint64
}

const (
	componentMetadataMaxBytes = 295 // owner length is at most 64 UTF-8 bytes
	componentCommitTimeBytes  = 8
	componentFieldNameBytes   = 3 // b/m/t
	componentProtocolReserve  = 78
	componentReferenceReserve = componentMetadataMaxBytes + componentCommitTimeBytes + componentFieldNameBytes + componentProtocolReserve // 384
	componentCipherOverhead   = 2 + 12 + 16                                                                                               // header + random nonce + GCM tag
)

func (m componentMetadata) marshal() ([]byte, error) {
	if m.Format != 10 || len(m.Owner) > 64 || m.Owner == "" {
		return nil, fmt.Errorf("%w: component metadata format/owner", errBadState)
	}
	for _, bank := range m.Banks {
		if bank > 1 {
			return nil, fmt.Errorf("%w: component bank", errBadState)
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
func decodeComponentMetadata(data []byte) (componentMetadata, error) {
	var m componentMetadata
	if len(data) > componentMetadataMaxBytes {
		return m, fmt.Errorf("%w: metadata size", errBadState)
	}
	r := bytes.NewReader(data)
	if err := binary.Read(r, binary.BigEndian, &m.Format); err != nil {
		return m, err
	}
	for i := range m.References {
		if _, err := io.ReadFull(r, m.References[i].Digest[:]); err != nil {
			return m, err
		}
		if err := binary.Read(r, binary.BigEndian, &m.References[i].KeyID); err != nil {
			return m, err
		}
		if err := binary.Read(r, binary.BigEndian, &m.References[i].Revision); err != nil {
			return m, err
		}
	}
	if err := binary.Read(r, binary.BigEndian, &m.OwnerEpoch); err != nil {
		return m, err
	}
	if err := binary.Read(r, binary.BigEndian, &m.StateVersion); err != nil {
		return m, err
	}
	if _, err := io.ReadFull(r, m.Banks[:]); err != nil {
		return m, err
	}
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return m, err
	}
	if int(n) > r.Len() || n > 64 {
		return m, fmt.Errorf("%w: metadata owner", errBadState)
	}
	owner := make([]byte, n)
	if _, err := io.ReadFull(r, owner); err != nil {
		return m, err
	}
	m.Owner = string(owner)
	if err := binary.Read(r, binary.BigEndian, &m.SetupGeneration); err != nil {
		return m, err
	}
	canonical, err := m.marshal()
	if err != nil {
		return m, err
	}
	if !bytes.Equal(canonical, data) {
		return m, fmt.Errorf("%w: noncanonical metadata", errBadState)
	}
	return m, nil
}

func componentKey(master []byte, kind byte, purpose string) ([]byte, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("%w: component key length", errBadState)
	}
	return hkdf.Key(sha256.New, master, nil, fmt.Sprintf("relais/session/v10/%d/%s", kind, purpose), 32)
}
func componentDigest(master []byte, kind byte, data []byte) ([32]byte, error) {
	var digest [32]byte
	key, err := componentKey(master, kind, "digest")
	if err != nil {
		return digest, err
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	copy(digest[:], h.Sum(nil))
	return digest, nil
}
func componentRootDigest(master []byte, version uint32, work, history componentReference) ([32]byte, error) {
	var b bytes.Buffer
	// Canonical length-prefixed tuple; key IDs accompany retained digests.
	for _, part := range [][]byte{binary.BigEndian.AppendUint32(nil, version), referenceBytes(work), referenceBytes(history)} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(part)))
		b.Write(part)
	}
	return componentDigest(master, componentRoot, b.Bytes())
}
func referenceBytes(ref componentReference) []byte {
	b := []byte{ref.KeyID}
	b = binary.BigEndian.AppendUint64(b, ref.Revision)
	return append(b, ref.Digest[:]...)
}
func componentWholeDigest(master []byte, metadata componentMetadata, transport []byte) ([32]byte, error) {
	// The whole-checkpoint digest excludes only itself to avoid a circular hash.
	metadata.References[4].Digest = [32]byte{}
	m, err := metadata.marshal()
	if err != nil {
		return [32]byte{}, err
	}
	return componentDigest(master, componentCheckpoint, append(m, transport...))
}

func componentAAD(sessionID string, kind byte, m componentMetadata, header []byte) ([]byte, error) {
	if sessionID == "" || len(sessionID) > componentDecodeLimit {
		return nil, fmt.Errorf("%w: session identity", errBadState)
	}
	var b bytes.Buffer
	b.WriteString("relais/session/v10/aad")
	_ = binary.Write(&b, binary.BigEndian, uint32(len(sessionID)))
	b.WriteString(sessionID)
	b.WriteByte(kind)
	_ = binary.Write(&b, binary.BigEndian, m.SetupGeneration)
	b.Write(header)
	if kind == componentTransport {
		packed, err := m.marshal()
		if err != nil {
			return nil, err
		}
		b.Write(packed)
	} else {
		index := int(kind) - 1
		if kind == componentWork {
			index = 1
		}
		if kind == componentHistory {
			index = 2
		}
		if index < 0 || index > 2 {
			return nil, fmt.Errorf("%w: component kind", errBadState)
		}
		b.WriteByte(m.Banks[index])
		b.Write(referenceBytes(m.References[index]))
		if kind != componentSetup {
			_ = binary.Write(&b, binary.BigEndian, m.StateVersion)
		}
	}
	return b.Bytes(), nil
}
func componentAEAD(master []byte, kind byte) (cipher.AEAD, error) {
	key, err := componentKey(master, kind, "encryption")
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func sealComponent(master []byte, keyID byte, sessionID string, kind byte, m componentMetadata, payload []byte) ([]byte, error) {
	aead, err := componentAEAD(master, kind)
	if err != nil {
		return nil, err
	}
	header := []byte{1, keyID}
	aad, err := componentAAD(sessionID, kind, m, header)
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
func openComponent(master []byte, keyID byte, sessionID string, kind byte, m componentMetadata, sealed []byte) ([]byte, error) {
	if len(sealed) < componentCipherOverhead || sealed[0] != 1 || sealed[1] != keyID {
		return nil, fmt.Errorf("%w: encryption header", errBadState)
	}
	aead, err := componentAEAD(master, kind)
	if err != nil {
		return nil, err
	}
	aad, err := componentAAD(sessionID, kind, m, sealed[:2])
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, sealed[2:14], sealed[14:], aad)
}

func sealSessionComponents(master []byte, keyID byte, sessionID string, m componentMetadata, c sessionComponents) (sessionComponents, componentMetadata, error) {
	var sealed sessionComponents
	for i, part := range [][]byte{c.Setup, c.Work, c.History} {
		kind := []byte{componentSetup, componentWork, componentHistory}[i]
		digest, err := componentDigest(master, kind, part)
		if err != nil {
			return sealed, m, err
		}
		m.References[i].Digest, m.References[i].KeyID = digest, keyID
	}
	root, err := componentRootDigest(master, m.StateVersion, m.References[1], m.References[2])
	if err != nil {
		return sealed, m, err
	}
	m.References[3].Digest, m.References[3].KeyID = root, keyID
	m.References[4].KeyID = keyID
	whole, err := componentWholeDigest(master, m, c.Transport)
	if err != nil {
		return sealed, m, err
	}
	m.References[4].Digest = whole
	for _, part := range []struct {
		kind  byte
		plain []byte
		out   *[]byte
	}{{componentSetup, c.Setup, &sealed.Setup}, {componentTransport, c.Transport, &sealed.Transport}, {componentWork, c.Work, &sealed.Work}, {componentHistory, c.History, &sealed.History}} {
		*part.out, err = sealComponent(master, keyID, sessionID, part.kind, m, part.plain)
		if err != nil {
			return sessionComponents{}, m, err
		}
	}
	return sealed, m, nil
}

func openSessionComponents(keys map[uint8][]byte, sessionID string, m componentMetadata, sealed sessionComponents) (sessionComponents, error) {
	var c sessionComponents
	for _, part := range []struct {
		kind   byte
		index  int
		sealed []byte
		out    *[]byte
	}{{componentSetup, 0, sealed.Setup, &c.Setup}, {componentTransport, 4, sealed.Transport, &c.Transport}, {componentWork, 1, sealed.Work, &c.Work}, {componentHistory, 2, sealed.History, &c.History}} {
		ref := m.References[part.index]
		plain, err := openComponent(keys[ref.KeyID], ref.KeyID, sessionID, part.kind, m, part.sealed)
		if err != nil {
			return sessionComponents{}, err
		}
		var digest [32]byte
		if part.kind == componentTransport {
			digest, err = componentWholeDigest(keys[ref.KeyID], m, plain)
		} else {
			digest, err = componentDigest(keys[ref.KeyID], part.kind, plain)
		}
		if err != nil {
			return sessionComponents{}, err
		}
		if !hmac.Equal(digest[:], ref.Digest[:]) {
			return sessionComponents{}, fmt.Errorf("%w: component digest", errBadState)
		}
		*part.out = plain
	}
	root, err := componentRootDigest(keys[m.References[3].KeyID], m.StateVersion, m.References[1], m.References[2])
	if err != nil {
		return sessionComponents{}, err
	}
	if !hmac.Equal(root[:], m.References[3].Digest[:]) {
		return sessionComponents{}, fmt.Errorf("%w: app root digest", errBadState)
	}
	return c, nil
}
