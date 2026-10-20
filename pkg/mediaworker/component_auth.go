package mediaworker

import (
	"fmt"

	componentcodec "github.com/relaishq/relais/internal/componentcodec"
)

type componentReference = componentcodec.Reference
type componentMetadata componentcodec.Metadata

func (m componentMetadata) marshal() ([]byte, error) {
	b, err := componentcodec.Metadata(m).Marshal()
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return b, err
}

const (
	componentMetadataMaxBytes = componentcodec.MetadataMaxBytes
	componentCommitTimeBytes  = componentcodec.CommitTimeBytes
	componentFieldNameBytes   = componentcodec.FieldNameBytes
	componentProtocolReserve  = componentcodec.ProtocolReserve
	componentReferenceReserve = componentcodec.ReferenceReserve
	componentCipherOverhead   = componentcodec.CipherOverhead
)

func decodeComponentMetadata(data []byte) (componentMetadata, error) {
	value, err := componentcodec.DecodeMetadata(data)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return componentMetadata(value), err
}
func componentKey(master []byte, sessionID string, kind byte, purpose string) ([]byte, error) {
	value, err := componentcodec.Key(master, sessionID, kind, purpose)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return value, err
}
func componentDigest(master []byte, sessionID string, kind byte, data []byte) ([32]byte, error) {
	value, err := componentcodec.Digest(master, sessionID, kind, data)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return value, err
}
func sealComponent(master []byte, keyID byte, sessionID string, kind byte, m componentMetadata, payload []byte) ([]byte, error) {
	value, err := componentcodec.Seal(master, keyID, sessionID, kind, componentcodec.Metadata(m), payload)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return value, err
}
func openComponent(master []byte, keyID byte, sessionID string, kind byte, m componentMetadata, sealed []byte) ([]byte, error) {
	value, err := componentcodec.Open(master, keyID, sessionID, kind, componentcodec.Metadata(m), sealed)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return value, err
}
func sealSessionComponents(master []byte, keyID byte, id string, m componentMetadata, c sessionComponents) (sessionComponents, componentMetadata, error) {
	sealed, metadata, err := componentcodec.SealAll(master, keyID, id, componentcodec.Metadata(m), componentcodec.Components(c))
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return sessionComponents(sealed), componentMetadata(metadata), err
}
func openSessionComponents(keys map[uint8][]byte, id string, m componentMetadata, c sessionComponents) (sessionComponents, error) {
	plain, err := componentcodec.OpenAll(keys, id, componentcodec.Metadata(m), componentcodec.Components(c))
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return sessionComponents(plain), err
}
