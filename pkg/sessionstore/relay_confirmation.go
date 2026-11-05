package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Relay confirmation modes (ADR 0016 sections 5, 7 and 10). The mode is
// accepted at create and fixed for the whole call.
const (
	RelayConfirmNone     = "none"
	RelayConfirmDelivery = "delivery" // C only: caller receiver reports
	RelayConfirmDecode   = "decode"   // A: caller decoded-media report
)

// Relay attempt phases. Confirmed, expired and aborted are terminal for the
// attempt's generation; a later attempt always uses a higher generation.
const (
	RelayPhasePending   = "pending"
	RelayPhaseSwitched  = "switched"
	RelayPhaseConfirmed = "confirmed"
	RelayPhaseExpired   = "expired"
	RelayPhaseAborted   = "aborted"
)

// Bounds keep the record small; the session hash is read on every check.
const (
	maxRelayConfirmationBytes = 4096
	maxRelayMIDs              = 16
	maxRelayMIDLength         = 32
	maxRelayTriedSlots        = 32
)

var ErrRelayConfirmationInvalid = errors.New("sessionstore: invalid relay confirmation")

// ErrRelayAttemptExpired refuses a confirmation write that would land after
// its deadline, measured by the store clock inside the atomic write.
var ErrRelayAttemptExpired = errors.New("sessionstore: relay attempt deadline passed")

// ErrRelayTooEarly refuses a write whose store-clock lower bound (an episode
// deadline) has not been reached inside the atomic write.
var ErrRelayTooEarly = errors.New("sessionstore: relay episode deadline not reached")

// Relay episode outcomes (ADR 0016 section 7). The first write that sets an
// outcome for an episode token wins; both are final for that token, and
// ended is final for the live session.
const (
	RelayEpisodeOpen      = "open"
	RelayEpisodeCompleted = "completed"
	RelayEpisodeEnded     = "ended"
)

// RelayEpisode is the call's durable episode decision (relay_episode field).
// Seq counts every final decision about the call's relay outcome (begins,
// completions and confirmations without an episode); it never decreases, so a
// stale proposal for seq+1 can never begin after any later decision.
type RelayEpisode struct {
	Seq           uint64    `json:"seq"`
	Token         string    `json:"token,omitempty"`
	StoreDeadline time.Time `json:"store_deadline,omitzero"`
	Outcome       string    `json:"outcome,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Detail        string    `json:"detail,omitempty"`
}

// Open reports an episode that has not decided yet.
func (e *RelayEpisode) Open() bool { return e != nil && e.Outcome == RelayEpisodeOpen }

// RelayAttempt is the bounded relay_confirmation_state record. It names the
// current attempt only; supersession replaces it and keeps the episode fields
// (Tried, Detected, EpisodeDeadline).
type RelayAttempt struct {
	Generation      uint64    `json:"generation"`
	Slot            string    `json:"slot"`
	Phase           string    `json:"phase"`
	Required        []string  `json:"required"`
	Decoded         []string  `json:"decoded,omitempty"`
	Delivered       []string  `json:"delivered,omitempty"`
	Kind            string    `json:"kind"` // planned or recovery
	Tried           []string  `json:"tried,omitempty"`
	CommitAt        time.Time `json:"commit_at,omitzero"`
	AttemptBound    time.Time `json:"attempt_bound,omitzero"`
	PlannedDeadline time.Time `json:"planned_deadline,omitzero"`
	Detected        time.Time `json:"detected,omitzero"`
	EpisodeDeadline time.Time `json:"episode_deadline,omitzero"`
	// StoreDeadline is Outer converted to the store clock when the attempt
	// was issued or adopted. Switch commits carry store time, so the two are
	// compared in one clock domain.
	StoreDeadline time.Time `json:"store_deadline,omitzero"`
	// StoreBound is the evidence bound in store-clock terms, fixed when the
	// switch is observed: StoreDeadline, or less for the A attempt bound. It is
	// authoritative; local times above only trigger earlier local decisions.
	StoreBound time.Time `json:"store_bound,omitzero"`
	// Episode is the token of the episode that owns this attempt, or empty.
	// Only proof on an attempt owned by an open episode completes it.
	Episode string `json:"episode,omitempty"`
}

// EvidenceBound is the store-clock time after which no A or C may be retained.
func (t RelayAttempt) EvidenceBound() time.Time {
	if !t.StoreBound.IsZero() {
		return t.StoreBound
	}
	return t.StoreDeadline
}

// RelayConfirmation is the per-call durable confirmation contract: the
// accepted mode and the current attempt. The zero value means a call created
// before this contract (no stored mode).
type RelayConfirmation struct {
	Mode    string
	Attempt *RelayAttempt
	Episode *RelayEpisode
	// Commit is maintained by the assignment writes. Reads return it with the
	// record; a confirmation write compares it and never changes it.
	Commit RelayCommit
}

// Outer is the attempt's applicable outer deadline in control-local time.
// The earlier of the two applies: an episode that adopts a planned attempt
// never extends it.
func (t RelayAttempt) Outer() time.Time {
	if t.EpisodeDeadline.IsZero() || !t.PlannedDeadline.IsZero() && t.PlannedDeadline.Before(t.EpisodeDeadline) {
		return t.PlannedDeadline
	}
	return t.EpisodeDeadline
}

// Bound is the deadline for new evidence: the attempt bound once switched,
// otherwise the outer deadline.
func (t RelayAttempt) Bound() time.Time {
	if !t.AttemptBound.IsZero() {
		return t.AttemptBound
	}
	return t.Outer()
}

// Aborted reports that the attempt's generation was consumed without a switch.
func (c RelayConfirmation) Aborted() bool {
	t := c.Attempt
	return t != nil && (t.Phase == RelayPhaseAborted || c.Commit.Retired >= t.Generation && c.Commit.Switched != t.Generation)
}

// Switched reports a real worker switch commit for the attempt, at or before
// its outer deadline, that the assignment still names.
func (c RelayConfirmation) Switched(a RelayAssignment) bool {
	t := c.Attempt
	if t == nil || c.Commit.Switched != t.Generation || a.Pending != "" || a.Slot != t.Slot || a.Generation != t.Generation {
		return false
	}
	return t.StoreDeadline.IsZero() || !c.Commit.SwitchedAt.After(t.StoreDeadline)
}

// RelayConfirmations stores confirmation state in the call's session hash, next
// to the relay assignment. CASRelayConfirmation compares the whole assignment
// and the whole record, so a write is ordered against the worker's switch
// commit, supersession, abort and expiry. A store-side control-epoch fence
// (#323) would add one more compared field to the same atomic write.
type RelayConfirmations interface {
	RelayAssignments
	RelayConfirmation(context.Context, string) (RelayAssignment, RelayConfirmation, error)
	// CASRelayConfirmation also compares from.Commit and from.Episode.
	// Non-zero notBefore and notAfter are store-clock times, enforced inside
	// the same atomic write.
	CASRelayConfirmation(ctx context.Context, id string, at RelayAssignment, from, to RelayConfirmation, notBefore, notAfter time.Time) error
}

func sameCommit(a, b RelayCommit) bool {
	return a.Switched == b.Switched && a.Retired == b.Retired && a.SwitchedAt.UnixMilli() == b.SwitchedAt.UnixMilli()
}

// Covers reports whether set contains every required MID. An empty required
// set never confirms (ADR 0016 section 7).
func Covers(set, required []string) bool {
	if len(required) == 0 {
		return false
	}
	for _, mid := range required {
		if !slices.Contains(set, mid) {
			return false
		}
	}
	return true
}

// Confirmed reports whether the durable record and assignment prove the
// attempt: the matching switch commit plus full evidence for the mode. This is
// the single completion predicate. Expiry and abort writes compare the same
// assignment and record, so neither can land after this becomes true.
func (c RelayConfirmation) Confirmed(a RelayAssignment) bool {
	t := c.Attempt
	if t == nil || t.Phase == RelayPhaseExpired || c.Aborted() {
		return false
	}
	if t.Phase == RelayPhaseConfirmed {
		return true
	}
	// A generation retirement is never a switch, even on the same slot.
	if !c.Switched(a) {
		return false
	}
	switch c.Mode {
	case RelayConfirmDecode:
		return Covers(t.Decoded, t.Required)
	case RelayConfirmDelivery:
		return Covers(t.Delivered, t.Required)
	}
	return false
}

func encodeRelayConfirmation(c RelayConfirmation) (mode, state, episode string, err error) {
	if episode, err = encodeRelayEpisode(c); err != nil {
		return "", "", "", err
	}
	mode, state, err = encodeRelayAttempt(c)
	return mode, state, episode, err
}

func encodeRelayEpisode(c RelayConfirmation) (string, error) {
	e := c.Episode
	if e == nil {
		return "", nil
	}
	if c.Mode != RelayConfirmDelivery && c.Mode != RelayConfirmDecode || len(e.Token) > 64 || len(e.Reason) > 64 || len(e.Detail) > 64 {
		return "", ErrRelayConfirmationInvalid
	}
	switch e.Outcome {
	case "", RelayEpisodeOpen, RelayEpisodeCompleted, RelayEpisodeEnded:
	default:
		return "", ErrRelayConfirmationInvalid
	}
	if e.Outcome == RelayEpisodeOpen && (e.Token == "" || e.StoreDeadline.IsZero()) {
		return "", ErrRelayConfirmationInvalid
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", ErrRelayConfirmationInvalid
	}
	return string(data), nil
}

func encodeRelayAttempt(c RelayConfirmation) (mode, state string, err error) {
	switch c.Mode {
	case "", RelayConfirmNone, RelayConfirmDelivery, RelayConfirmDecode:
	default:
		return "", "", ErrRelayConfirmationInvalid
	}
	if c.Attempt == nil {
		return c.Mode, "", nil
	}
	t := c.Attempt
	if c.Mode == "" || c.Mode == RelayConfirmNone || t.Slot == "" || len(t.Slot) > 256 ||
		len(t.Required) > maxRelayMIDs || len(t.Decoded) > maxRelayMIDs || len(t.Delivered) > maxRelayMIDs || len(t.Tried) > maxRelayTriedSlots {
		return "", "", ErrRelayConfirmationInvalid
	}
	switch t.Phase {
	case RelayPhasePending, RelayPhaseSwitched, RelayPhaseConfirmed, RelayPhaseExpired, RelayPhaseAborted:
	default:
		return "", "", ErrRelayConfirmationInvalid
	}
	for _, set := range [][]string{t.Required, t.Decoded, t.Delivered} {
		for _, mid := range set {
			if mid == "" || len(mid) > maxRelayMIDLength {
				return "", "", ErrRelayConfirmationInvalid
			}
		}
	}
	data, err := json.Marshal(t)
	if err != nil || len(data) > maxRelayConfirmationBytes {
		return "", "", ErrRelayConfirmationInvalid
	}
	return c.Mode, string(data), nil
}

func decodeRelayConfirmation(mode, state, episode string) (RelayConfirmation, error) {
	c := RelayConfirmation{Mode: mode}
	if state != "" {
		c.Attempt = &RelayAttempt{}
		if err := json.Unmarshal([]byte(state), c.Attempt); err != nil {
			return RelayConfirmation{}, ErrRelayConfirmationInvalid
		}
	}
	if episode != "" {
		c.Episode = &RelayEpisode{}
		if err := json.Unmarshal([]byte(episode), c.Episode); err != nil {
			return RelayConfirmation{}, ErrRelayConfirmationInvalid
		}
	}
	if _, _, _, err := encodeRelayConfirmation(c); err != nil {
		return RelayConfirmation{}, err
	}
	return c, nil
}

// The mode is set once. A record never returns to "no attempt"; it never
// moves to a lower generation; a terminal phase is final for its generation.
// The episode seq never decreases, and ended is final for the live session.
func validRelayConfirmationTransition(from, to RelayConfirmation) bool {
	if from.Mode != "" && to.Mode != from.Mode {
		return false
	}
	if from.Episode != nil {
		if to.Episode == nil || to.Episode.Seq < from.Episode.Seq {
			return false
		}
		if from.Episode.Outcome == RelayEpisodeEnded && !sameRelayEpisode(*to.Episode, *from.Episode) {
			return false
		}
	}
	if from.Attempt == nil {
		return true
	}
	if to.Attempt == nil || to.Attempt.Generation < from.Attempt.Generation {
		return false
	}
	if to.Attempt.Generation == from.Attempt.Generation {
		switch from.Attempt.Phase {
		case RelayPhaseConfirmed, RelayPhaseExpired, RelayPhaseAborted:
			return to.Attempt.Phase == from.Attempt.Phase
		}
	}
	return true
}

// sameRelayEpisode compares episodes with time.Equal for deadlines.
func sameRelayEpisode(a, b RelayEpisode) bool {
	return a.Seq == b.Seq && a.Token == b.Token && a.StoreDeadline.Equal(b.StoreDeadline) && a.Outcome == b.Outcome && a.Reason == b.Reason && a.Detail == b.Detail
}

func (m *Memory) RelayConfirmation(ctx context.Context, id string) (RelayAssignment, RelayConfirmation, error) {
	if err := ctx.Err(); err != nil {
		return RelayAssignment{}, RelayConfirmation{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	lease, ok := m.leases[id]
	if !ok || !time.Now().Before(lease.ExpiresAt) {
		return RelayAssignment{}, RelayConfirmation{}, ErrNotFound
	}
	saved := m.relayConfirmations[id]
	c, err := decodeRelayConfirmation(saved[0], saved[1], saved[2])
	c.Commit = m.relayCommits[id]
	return m.relayAssignments[id], c, err
}

func (m *Memory) CASRelayConfirmation(ctx context.Context, id string, at RelayAssignment, from, to RelayConfirmation, notBefore, notAfter time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromMode, fromState, fromEpisode, err := encodeRelayConfirmation(from)
	if err != nil {
		return err
	}
	toMode, toState, toEpisode, err := encodeRelayConfirmation(to)
	if err != nil {
		return err
	}
	if !validRelayConfirmationTransition(from, to) {
		return ErrRelayTransition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[id]
	if !ok || !time.Now().Before(lease.ExpiresAt) {
		return ErrNotFound
	}
	if m.relayAssignments[id] != at || m.relayConfirmations[id] != [3]string{fromMode, fromState, fromEpisode} || !sameCommit(m.relayCommits[id], from.Commit) {
		return ErrRelayConflict
	}
	now := time.Now()
	if !notAfter.IsZero() && now.After(notAfter) {
		return ErrRelayAttemptExpired
	}
	if !notBefore.IsZero() && now.Before(notBefore) {
		return ErrRelayTooEarly
	}
	if m.relayConfirmations == nil {
		m.relayConfirmations = make(map[string][3]string)
	}
	m.relayConfirmations[id] = [3]string{toMode, toState, toEpisode}
	return nil
}

var _ RelayConfirmations = (*Memory)(nil)
var _ RelayConfirmations = (*Redis)(nil)
