package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	MaxControlRecords     = 1024
	MaxControlRecordBytes = 4096
	MaxControlIntents     = 8192
)

var ErrControlLimit = errors.New("sessionstore: control metadata limit exceeded")

// ControlLease fences the single controller's metadata. It grants no session
// ownership. Expired tenures cannot renew; epoch history survives release.
type ControlLease struct {
	Holder    string    `json:"holder"`
	Epoch     uint64    `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ControlIntent contains only coordination metadata, never media/key material.
// Candidate is saved before a transfer can reach Redis. A successor settles it
// even when the old process never received the transition's reply.
type ControlIntent struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Kind      string    `json:"kind"`
	Started   time.Time `json:"started"`
	Lease     Lease     `json:"lease"`
	Candidate *Lease    `json:"candidate,omitempty"`
}

// ControlStore is the restart journal. Control-wide records use one slot;
// intents use the session's slot. No operation spans those slots. Callers must
// hold the control lease and persist an intent before starting its effects.
type ControlStore interface {
	ClaimControl(context.Context, string, time.Duration) (ControlLease, error)
	RenewControl(context.Context, ControlLease, time.Duration) (ControlLease, error)
	ReleaseControl(context.Context, ControlLease) error
	ControlRecords(context.Context, ControlLease) (map[string]json.RawMessage, error)
	PutControlRecord(context.Context, ControlLease, string, json.RawMessage) error
	PutControlIntent(context.Context, ControlIntent) error
	ControlIntents(context.Context) ([]ControlIntent, error)
	DeleteControlIntent(context.Context, string, string) error
}

type transitionIntentKey struct{}
type transitionIntent struct{ ID, Token string }

// WithTransitionIntent links Transfer to an already persisted operation. The
// store saves the allocated candidate before publishing any ownership effect.
func WithTransitionIntent(ctx context.Context, id, token string) context.Context {
	return context.WithValue(ctx, transitionIntentKey{}, transitionIntent{ID: id, Token: token})
}

func validControl(holder string, ttl time.Duration) bool {
	return holder != "" && len(holder) <= 128 && ttl >= time.Millisecond && ttl <= 30*time.Second
}
func controlRecordValid(key string, data []byte) bool {
	return key != "" && len(key) <= 256 && len(data) <= MaxControlRecordBytes && (len(data) == 0 || json.Valid(data))
}
func intentBody(intent ControlIntent) ([]byte, error) {
	if intent.ID == "" || len(intent.ID) > 256 || intent.Token == "" || len(intent.Token) > 128 || intent.Kind == "" {
		return nil, errors.New("sessionstore: invalid control intent")
	}
	data, err := json.Marshal(intent)
	if err == nil && len(data) > MaxControlRecordBytes {
		err = ErrControlLimit
	}
	return data, err
}

func (m *Memory) ClaimControl(ctx context.Context, holder string, ttl time.Duration) (ControlLease, error) {
	if err := ctx.Err(); err != nil {
		return ControlLease{}, err
	}
	if !validControl(holder, ttl) {
		return ControlLease{}, errors.New("sessionstore: invalid control lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if now.Before(m.controlLease.ExpiresAt) {
		if m.controlLease.Holder == holder {
			m.controlLease.ExpiresAt = now.Add(ttl)
			return m.controlLease, nil
		}
		return ControlLease{}, ErrLeaseHeld
	}
	m.controlLease = ControlLease{Holder: holder, Epoch: m.controlLease.Epoch + 1, ExpiresAt: now.Add(ttl)}
	return m.controlLease, nil
}
func (m *Memory) RenewControl(ctx context.Context, lease ControlLease, ttl time.Duration) (ControlLease, error) {
	if err := ctx.Err(); err != nil {
		return ControlLease{}, err
	}
	if !validControl(lease.Holder, ttl) {
		return ControlLease{}, errors.New("sessionstore: invalid control lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.holdsControl(lease) {
		return ControlLease{}, ErrLeaseLost
	}
	m.controlLease.ExpiresAt = time.Now().Add(ttl)
	return m.controlLease, nil
}
func (m *Memory) holdsControl(lease ControlLease) bool {
	return lease.Holder != "" && m.controlLease.Holder == lease.Holder && m.controlLease.Epoch == lease.Epoch && time.Now().Before(m.controlLease.ExpiresAt)
}
func (m *Memory) ReleaseControl(ctx context.Context, lease ControlLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.holdsControl(lease) {
		return ErrLeaseLost
	}
	m.controlLease.Holder = ""
	m.controlLease.ExpiresAt = time.Time{}
	return nil
}
func (m *Memory) ControlRecords(ctx context.Context, lease ControlLease) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.holdsControl(lease) {
		return nil, ErrLeaseLost
	}
	out := make(map[string]json.RawMessage, len(m.controlRecords))
	for k, v := range m.controlRecords {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out, nil
}
func (m *Memory) PutControlRecord(ctx context.Context, lease ControlLease, key string, data json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !controlRecordValid(key, data) {
		return ErrControlLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.holdsControl(lease) {
		return ErrLeaseLost
	}
	if len(data) == 0 {
		delete(m.controlRecords, key)
		return nil
	}
	if m.controlRecords == nil {
		m.controlRecords = map[string]json.RawMessage{}
	}
	if _, ok := m.controlRecords[key]; !ok && len(m.controlRecords) >= MaxControlRecords {
		return ErrControlLimit
	}
	m.controlRecords[key] = append(json.RawMessage(nil), data...)
	return nil
}
func (m *Memory) PutControlIntent(ctx context.Context, intent ControlIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := intentBody(intent); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controlIntents == nil {
		m.controlIntents = map[string]ControlIntent{}
	}
	if old, ok := m.controlIntents[intent.ID]; ok && old.Token != intent.Token {
		return ErrLeaseHeld
	}
	if _, ok := m.controlIntents[intent.ID]; !ok && len(m.controlIntents) >= MaxControlIntents {
		return ErrControlLimit
	}
	if old, ok := m.controlIntents[intent.ID]; ok && intent.Candidate == nil {
		intent.Candidate = old.Candidate
	}
	m.controlIntents[intent.ID] = cloneControlIntent(intent)
	return nil
}
func cloneControlIntent(intent ControlIntent) ControlIntent {
	if intent.Candidate != nil {
		candidate := *intent.Candidate
		intent.Candidate = &candidate
	}
	return intent
}
func (m *Memory) ControlIntents(ctx context.Context) ([]ControlIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ControlIntent, 0, len(m.controlIntents))
	for _, v := range m.controlIntents {
		out = append(out, cloneControlIntent(v))
	}
	return out, nil
}
func (m *Memory) DeleteControlIntent(ctx context.Context, id, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.controlIntents[id]; ok && old.Token != token {
		return ErrLeaseLost
	}
	delete(m.controlIntents, id)
	return nil
}

// Memory transitions are synchronous under one lock: no command can arrive
// after a negative settlement. Advance its epoch to fence the candidate anyway.
func (m *Memory) Settle(ctx context.Context, candidate Lease) (Lease, bool, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.leases[candidate.SessionID]; ok && same(current, candidate) && time.Now().Before(current.ExpiresAt) {
		return current, true, nil
	}
	m.epoch = max(m.epoch, candidate.Epoch)
	return Lease{}, false, nil
}
