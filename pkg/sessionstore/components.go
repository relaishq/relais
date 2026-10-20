package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	codec "github.com/relaishq/relais/internal/componentcodec"
)

// ComponentMetadata is the frozen v10 authenticated reference tuple.
type ComponentMetadata = codec.Metadata

// Components contains canonical v10 codec bytes, never application JSON or Redis pointers.
type Components = codec.Components

var (
	ErrUnsafeCheckpoint  = errors.New("sessionstore: unsafe component checkpoint")
	ErrComponentCapacity = errors.New("sessionstore: component write exceeds budget")
	ErrSetupChanged      = errors.New("sessionstore: connection setup changed")
	ErrCheckpointLayout  = errors.New("sessionstore: incompatible checkpoint layout")
)

// ComponentCommit is a compare-and-set of one complete capture. Expected is
// nil only at initialization. Copy the metadata from the last acknowledged
// tuple, including its root; a stale capture must never revert newer app data.
// WriteBudgetBytes limits ciphertext and metadata written, not wire traffic.
// Zero means no additional write-budget restriction. Rotation is deferred when
// only its extra bytes exceed the budget. Ordinary writes remain admissible.
type ComponentCommit struct {
	Components       Components
	StateVersion     uint32
	Expected         *ComponentMetadata
	WriteBudgetBytes int
	// FinalizeTransport embeds the exact selected write charge (including rotation)
	// before sealing. It must be pure and preserve encoded transport length.
	FinalizeTransport func(writtenBytes int) ([]byte, error)
}

// ComponentTuple is one authenticated primary read. Checkpoint and Lease are
// from the same script/lock as Components. Metadata identifies this exact capture.
type ComponentTuple struct {
	Components       Components
	Metadata         ComponentMetadata
	Lease            Lease
	Checkpoint       Checkpoint
	WrittenBytes     int
	RotationDeferred bool
}

// ComponentStore is additive; legacy v8/v9 Store operations keep their layout.
type ComponentStore interface {
	CommitComponents(context.Context, Lease, ComponentCommit) (ComponentTuple, error)
	ReadComponents(context.Context, string) (ComponentTuple, error)
	ComponentKeyInventory(context.Context) (map[byte]uint64, error)
}

var _ ComponentStore = (*Memory)(nil)
var _ ComponentStore = (*Redis)(nil)

type componentRecord struct {
	metadata ComponentMetadata
	sealed   Components
	stored   time.Time
}
type componentPreparation struct {
	record   componentRecord
	changed  [3]bool
	written  int
	deferred bool
	plain    Components
}

func componentBytes(c Components) [][]byte { return [][]byte{c.Setup, c.Work, c.History} }
func cloneComponents(c Components) Components {
	return Components{Setup: bytes.Clone(c.Setup), Transport: bytes.Clone(c.Transport), Work: bytes.Clone(c.Work), History: bytes.Clone(c.History)}
}
func metadataEqual(a, b ComponentMetadata) bool {
	x, _ := a.Marshal()
	y, _ := b.Marshal()
	return bytes.Equal(x, y)
}
func validComponents(id string, c Components) error {
	if err := codec.Validate(id, c); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsafeCheckpoint, err)
	}
	return nil
}
func validateComponentMetadata(m ComponentMetadata) error {
	seq := m.References[4].Revision
	if m.Format != 10 || m.SetupGeneration == 0 || m.OwnerEpoch == 0 || seq == 0 {
		return ErrUnsafeCheckpoint
	}
	for _, ref := range m.References {
		if ref.Revision == 0 || ref.Revision > seq {
			return ErrUnsafeCheckpoint
		}
	}
	_, err := m.Marshal()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnsafeCheckpoint, err)
	}
	return nil
}
func checkpointAt(stored, now time.Time) (Checkpoint, error) {
	if now.Before(stored) {
		return Checkpoint{}, ErrUnsafeCheckpointClock
	}
	return Checkpoint{StoredAt: stored, Now: now, Age: now.Sub(stored)}, nil
}
func (r *Redis) openRecord(id string, record componentRecord) (Components, error) {
	if err := validateComponentMetadata(record.metadata); err != nil {
		return Components{}, err
	}
	c, err := codec.OpenAll(r.keysByID, id, record.metadata, record.sealed)
	if err != nil {
		return Components{}, fmt.Errorf("%w: %w", ErrUnsafeCheckpoint, err)
	}
	if err = validComponents(id, c); err != nil {
		return Components{}, err
	}
	return c, nil
}
func (r *Redis) prepareComponents(lease Lease, request ComponentCommit, current *componentRecord, seq uint64) (componentPreparation, error) {
	var p componentPreparation
	if err := validComponents(lease.SessionID, request.Components); err != nil {
		return p, err
	}
	m := ComponentMetadata{Format: 10, Owner: lease.Worker.String(), OwnerEpoch: lease.Epoch, StateVersion: request.StateVersion, SetupGeneration: lease.Epoch}
	if current != nil {
		if err := validateComponentMetadata(current.metadata); err != nil {
			return p, err
		}
		if request.Expected == nil || !metadataEqual(*request.Expected, current.metadata) {
			return p, ErrStateSuperseded
		}
		m = current.metadata
		m.Owner = lease.Worker.String()
		m.OwnerEpoch = lease.Epoch
		m.StateVersion = request.StateVersion
	} else if request.Expected != nil {
		return p, ErrStateSuperseded
	}
	parts := componentBytes(request.Components)
	rotation := false
	for i, data := range parts {
		p.changed[i] = current == nil
		if current != nil {
			ref := m.References[i]
			digest, err := codec.Digest(r.keysByID[ref.KeyID], lease.SessionID, []byte{codec.Setup, codec.Work, codec.History}[i], data)
			if err != nil {
				return p, fmt.Errorf("%w: %w", ErrUnsafeCheckpoint, err)
			}
			p.changed[i] = digest != ref.Digest
			if i == 0 && p.changed[i] {
				return p, ErrSetupChanged
			}
			p.changed[i] = p.changed[i] || (i > 0 && current.metadata.StateVersion != request.StateVersion)
			if ref.KeyID != r.activeKey {
				rotation = true
			}
		}
	}

	// Build ordinary commit first. Rotation cannot turn an otherwise admissible
	// capture into an application capacity refusal.
	build := func(rotate bool) (componentPreparation, error) {
		q := p
		mm := m
		if current != nil {
			q.record.sealed = cloneComponents(current.sealed)
		}
		outputs := []*[]byte{&q.record.sealed.Setup, &q.record.sealed.Work, &q.record.sealed.History}
		for i, data := range parts {
			if rotate && mm.References[i].KeyID != r.activeKey {
				q.changed[i] = true
			}
			if !q.changed[i] {
				continue
			}
			if current != nil {
				mm.Banks[i] = 1 - current.metadata.Banks[i]
			}
			kind := []byte{codec.Setup, codec.Work, codec.History}[i]
			digest, err := codec.Digest(r.keysByID[r.activeKey], lease.SessionID, kind, data)
			if err != nil {
				return q, err
			}
			mm.References[i] = codec.Reference{Digest: digest, KeyID: r.activeKey, Revision: seq}
		}
		rootChanged := current == nil || mm.StateVersion != current.metadata.StateVersion || mm.References[1] != current.metadata.References[1] || mm.References[2] != current.metadata.References[2]
		if rootChanged {
			digest, err := codec.RootDigest(r.keysByID[r.activeKey], lease.SessionID, mm.StateVersion, mm.References[1], mm.References[2])
			if err != nil {
				return q, err
			}
			mm.References[3] = codec.Reference{Digest: digest, KeyID: r.activeKey, Revision: seq}
		}
		mm.References[4] = codec.Reference{KeyID: r.activeKey, Revision: seq}
		digest, err := codec.WholeDigest(r.keysByID[r.activeKey], lease.SessionID, mm, request.Components.Transport)
		if err != nil {
			return q, err
		}
		mm.References[4].Digest = digest
		for i, data := range parts {
			if q.changed[i] {
				*outputs[i], err = codec.Seal(r.keysByID[r.activeKey], r.activeKey, lease.SessionID, []byte{codec.Setup, codec.Work, codec.History}[i], mm, data)
				if err != nil {
					return q, err
				}
				q.written += len(*outputs[i])
			}
		}
		q.record.sealed.Transport, err = codec.Seal(r.keysByID[r.activeKey], r.activeKey, lease.SessionID, codec.Transport, mm, request.Components.Transport)
		if err != nil {
			return q, err
		}
		packed, err := mm.Marshal()
		if err != nil {
			return q, err
		}
		q.written += len(q.record.sealed.Transport) + len(packed) + codec.CommitTimeBytes + codec.FieldNameBytes
		q.record.metadata = mm
		return q, nil
	}
	ordinary, err := build(false)
	if err != nil {
		return p, err
	}
	if request.WriteBudgetBytes < 0 || (request.WriteBudgetBytes > 0 && ordinary.written > request.WriteBudgetBytes) {
		return p, ErrComponentCapacity
	}
	selected := ordinary
	if rotation {
		rotated, err := build(true)
		if err != nil {
			return p, err
		}
		if request.WriteBudgetBytes > 0 && rotated.written > request.WriteBudgetBytes {
			selected.deferred = true
		} else {
			selected = rotated
		}
	}
	if request.FinalizeTransport != nil {
		transport, err := request.FinalizeTransport(selected.written)
		if err != nil {
			return p, err
		}
		if len(transport) != len(request.Components.Transport) {
			return p, fmt.Errorf("%w: finalized transport length changed", ErrUnsafeCheckpoint)
		}
		request.Components.Transport = bytes.Clone(transport)
		if err := validComponents(lease.SessionID, request.Components); err != nil {
			return p, err
		}
		finalized, err := build(rotation && !selected.deferred)
		if err != nil {
			return p, err
		}
		if finalized.written != selected.written {
			return p, ErrUnsafeCheckpoint
		}
		finalized.deferred = selected.deferred
		selected = finalized
	}
	selected.plain = cloneComponents(request.Components)
	return selected, nil

}
func tupleFromRecord(r *Redis, id string, record componentRecord, lease Lease, now time.Time) (ComponentTuple, error) {
	c, err := r.openRecord(id, record)
	if err != nil {
		return ComponentTuple{}, err
	}
	if record.metadata.OwnerEpoch > lease.Epoch || (record.metadata.OwnerEpoch == lease.Epoch && record.metadata.Owner != lease.Worker.String()) {
		return ComponentTuple{}, ErrUnsafeCheckpoint
	}
	checkpoint, err := checkpointAt(record.stored, now)
	if err != nil {
		return ComponentTuple{}, err
	}
	return ComponentTuple{Components: c, Metadata: record.metadata, Lease: lease, Checkpoint: checkpoint}, nil
}
func sequenceNumber(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("%w: invalid sequence", ErrUnsafeCheckpoint)
	}
	return v, nil
}
