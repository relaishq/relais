package mediaworker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// SnapshotLimits are operator settings; registrations cannot change them.
// The encoded cap also bounds v1 component captures. Byte-rate and move quotas
// apply to v2 sessions. Zero fields resolve by session profile: ADR 0010
// for legacy sessions and ADR 0012 for v10 sessions.
type SnapshotLimits struct {
	MaxBytes            int
	BytesPerSecond      int
	BurstBytes          int
	MoveBytesPerSession int
	MoveBytesTotal      int64
}

// LegacyDefaults resolves the ADR 0010 profile and preserves explicit limits.
func (l SnapshotLimits) LegacyDefaults() SnapshotLimits { return l.defaults() }

func (l SnapshotLimits) defaults() SnapshotLimits {
	if l.MaxBytes == 0 {
		l.MaxBytes = 7168
	}
	if l.BytesPerSecond == 0 {
		l.BytesPerSecond = 81920
	}
	if l.BurstBytes == 0 {
		l.BurstBytes = 28672
	}
	if l.MoveBytesPerSession == 0 {
		l.MoveBytesPerSession = v2MoveSessionBytes
	}
	if l.MoveBytesTotal == 0 {
		l.MoveBytesTotal = v2MoveWorkerBytes
	}
	return l
}

// SplitDefaults resolves the gate-derived v10 profile. Explicit settings keep
// their meaning in both layouts; defaults are selected per session, not per worker.
func (l SnapshotLimits) SplitDefaults() SnapshotLimits { return l.splitDefaults() }

func (l SnapshotLimits) splitDefaults() SnapshotLimits {
	if l.MaxBytes == 0 {
		l.MaxBytes = 6144
	}
	if l.BytesPerSecond == 0 {
		l.BytesPerSecond = 70000
	}
	if l.BurstBytes == 0 {
		l.BurstBytes = 40960
	}
	return l.defaults()
}
func (s *session) snapshotLimits() SnapshotLimits {
	if s.componentMode {
		return s.worker.splitLimits.splitDefaults()
	}
	return s.worker.cfg.SnapshotLimits
}

// Validate checks limits at the stock worker's 100 ms snapshot interval.
func (l SnapshotLimits) Validate() error { return l.ValidateForInterval(100 * time.Millisecond) }

// ValidateForInterval reserves one maximum-size forced write per second beyond
// periodic transport checkpoints. Intent admission also preserves one required
// checkpoint in the bucket; excess forced work waits for replenishment.
func (l SnapshotLimits) ValidateForInterval(interval time.Duration) error {
	if err := l.validateResourceLimits(); err != nil {
		return err
	}
	if interval <= 0 {
		return fmt.Errorf("%w: snapshot interval must be positive", agent.ErrCapacity)
	}
	numerator := int64(l.MaxBytes) * int64(time.Second)
	periodic := numerator / int64(interval)
	if numerator%int64(interval) != 0 {
		periodic++
	}
	minimum := periodic + int64(l.MaxBytes)
	if int64(l.BytesPerSecond) < minimum {
		return fmt.Errorf("%w: -agent-snapshot-bytes-per-second must be at least %d for snapshot interval %s", agent.ErrCapacity, minimum, interval)
	}
	return nil
}

// ValidateSnapshotProfiles checks cadence and every declared v2 registration
// before startup. Both profiles must support cadence on split workers, but only
// split registration fit blocks their startup. Legacy fit warns at startup;
// split workers measure actual legacy snapshot bytes before restore. Factory alone stays opaque until session setup.
// Application agents are never constructed by this preflight.
func ValidateSnapshotProfiles(cfg Config) error {
	interval := cfg.SnapshotInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	profiles := []struct {
		name   string
		limits SnapshotLimits
		split  bool
	}{{"legacy", cfg.SnapshotLimits.LegacyDefaults(), false}}
	if cfg.SplitCheckpoints {
		profiles = append(profiles, struct {
			name   string
			limits SnapshotLimits
			split  bool
		}{"split", cfg.SnapshotLimits.SplitDefaults(), true})
	}
	names := make([]string, 0, len(cfg.Agent.Registrations))
	for name := range cfg.Agent.Registrations {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, profile := range profiles {
		if err := profile.limits.ValidateForInterval(interval); err != nil {
			return fmt.Errorf("%s session profile: %w; raise -agent-snapshot-bytes-per-second to cover periodic and forced writes; an explicit -agent-max-snapshot-bytes must also fit registrations in the new-call profile", profile.name, err)
		}
		if cfg.SplitCheckpoints && !profile.split {
			// A split-sized declaration need not fit legacy snapshots. New logs
			// this conservative adoption warning; ResumeSession checks actual bytes.
			continue
		}
		for _, name := range names {
			factory := cfg.Agent.Registrations[name]
			if factory == nil {
				return fmt.Errorf("%s session profile registration %q: %w: missing metadata adapter", profile.name, name, agent.ErrIncompatibleAgent)
			}
			adapter, ok := factory("").(*agenthost.Adapter)
			if !ok || adapter == nil {
				return fmt.Errorf("%s session profile registration %q: %w: expected v2 metadata adapter", profile.name, name, agent.ErrIncompatibleAgent)
			}
			r := adapter.Registration
			if err := ValidateAgentRegistration(r.Name, r.TaskKinds, r.Declared, profile.limits, profile.split); err != nil {
				return fmt.Errorf("%s session profile registration %q: %w", profile.name, name, err)
			}
		}
	}
	return nil
}

// warnLegacyRegistrationFits runs once at startup, after hard preflight and
// logger setup. Warnings describe legacy adoption, not new split admission.
func (w *Worker) warnLegacyRegistrationFits() {
	for _, factory := range w.cfg.Agent.Registrations {
		adapter, ok := factory("").(*agenthost.Adapter)
		if !ok || adapter == nil {
			continue // New already validates metadata adapters before start.
		}
		reg := adapter.Registration
		if err := validateLegacyRegistrationFit(reg, w.cfg.SnapshotLimits); err != nil {
			w.log.Warnf("legacy calls with registration %q may not be adoptable by this worker: %v", reg.Name, err)
		}
	}
}

// validateLegacyRegistrationFit is a conservative startup warning only.
// Restore admission measures the actual snapshot, not this declared estimate.
func validateLegacyRegistrationFit(reg agenthost.Registration, limits SnapshotLimits) error {
	if err := ValidateAgentRegistration(reg.Name, reg.TaskKinds, reg.Declared, limits, false); err != nil {
		return fmt.Errorf("%w: legacy session profile registration %q: %w", agent.ErrIncompatibleAgent, reg.Name, err)
	}
	return nil
}

// validateLegacySnapshotFit protects split workers adopting v8/v9 sessions.
// Measure the durable form, excluding the separately bounded move-only payload.
// Legacy pools keep their existing restore behavior, including source rollback.
func (w *Worker) validateLegacySnapshotFit(snap snapshot) error {
	if !w.cfg.SplitCheckpoints || snap.split != nil {
		return nil
	}
	snap.AgentMoveOnly = nil
	encoded, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	actual := len(encoded) + snapshotEnvelopeBytes
	cap := w.cfg.SnapshotLimits.LegacyDefaults().MaxBytes
	if actual > cap {
		return fmt.Errorf("%w: %w: %w: legacy snapshot for registration %q is %d encoded bytes (cap %d, short by %d bytes)", legacy.ErrRestore, agent.ErrIncompatibleAgent, agent.ErrStateTooLarge, snap.State.Agent.V2.Name, actual, cap, actual-cap)
	}
	return nil
}

// validateResourceLimits is independent of checkpoint cadence and agent kind.
func (l SnapshotLimits) validateResourceLimits() error {
	const maxByteFlag = 1<<30 - 1 // sums of two byte caps fit even a 32-bit int
	for _, v := range []struct {
		name    string
		value   int64
		maximum int64
	}{
		{"agent-max-snapshot-bytes", int64(l.MaxBytes), maxByteFlag},
		{"agent-snapshot-bytes-per-second", int64(l.BytesPerSecond), maxByteFlag},
		{"agent-snapshot-burst-bytes", int64(l.BurstBytes), maxByteFlag},
		{"agent-move-bytes-per-session", int64(l.MoveBytesPerSession), v2MoveSessionBytes},
		{"agent-move-bytes-total", l.MoveBytesTotal, 1 << 40},
	} {
		if v.value < 1 || v.value > v.maximum {
			return fmt.Errorf("%w: -%s must be between 1 and %d bytes", agent.ErrCapacity, v.name, v.maximum)
		}
	}
	if l.BurstBytes < 2*l.MaxBytes {
		return fmt.Errorf("%w: -agent-snapshot-burst-bytes must be at least %d", agent.ErrCapacity, 2*l.MaxBytes)
	}
	if l.MoveBytesTotal <= int64(l.MoveBytesPerSession)*8 {
		return fmt.Errorf("%w: -agent-move-bytes-total must be at least %d", agent.ErrCapacity, int64(l.MoveBytesPerSession)*8+1)
	}
	return nil
}

// effectiveAgentBounds resolves defaults and local ceilings without changing the
// declaration used as registration identity. State is never silently clamped.
func effectiveAgentBounds(declared agent.Limits, limits SnapshotLimits, split ...bool) agenthost.Bounds {
	if len(split) > 0 && split[0] {
		if declared.MaxStateBytes == 0 {
			declared.MaxStateBytes = 3072
		}
		if declared.MaxHistoryBytes == 0 {
			declared.MaxHistoryBytes = 8192
		}
	}
	b := agenthost.Bounds(declared).Defaults()
	if len(split) == 0 || !split[0] {
		b.MaxHistoryBytes = 0
	}
	b.MaxPendingBytes = min(b.MaxPendingBytes, v2HostMetadataBytes)
	b.MaxMailboxBytes = min(b.MaxMailboxBytes, limits.MoveBytesPerSession)
	b.MaxActiveTasks = min(b.MaxActiveTasks, 4)
	if b.MaxAudioBytes == 0 {
		b.MaxAudioBytes = b.MaxMailboxBytes
	} else {
		b.MaxAudioBytes = min(b.MaxAudioBytes, b.MaxMailboxBytes)
	}
	return b
}

// ValidateAgentRegistration checks declared crash-state capacity before startup.
// The measured transport baseline is an admission allowance only, never a claim
// about a live snapshot. Live admission and capture encode the actual snapshot.
func ValidateAgentRegistration(name string, kinds []string, bounds agent.Limits, limits SnapshotLimits, split ...bool) error {
	isSplit := len(split) > 0 && split[0]
	if isSplit {
		limits = limits.splitDefaults()
	} else {
		limits = limits.defaults()
	}
	declared := bounds
	bounds = agent.Limits(effectiveAgentBounds(declared, limits, isSplit))
	if declared.MaxHistoryBytes < 0 || declared.MaxHistoryBytes > componentMaxHistoryBytes || bounds.MaxStateBytes <= 0 || bounds.MaxStateBytes > 64<<10 || bounds.MaxPendingBytes <= 0 || bounds.MaxPendingBytes > v2HostMetadataBytes || bounds.MaxMailboxBytes <= 0 || bounds.MaxMailboxBytes > limits.MoveBytesPerSession || bounds.MaxActiveTasks <= 0 || bounds.MaxAudioBytes < 0 || bounds.MaxAudioDuration < 0 {
		return fmt.Errorf("%w: invalid limits for %q", agent.ErrIncompatibleAgent, name)
	}
	if isSplit && limits.BurstBytes < 2*splitComponentBudgets(limits.MaxBytes, bounds.MaxHistoryBytes).Total {
		return fmt.Errorf("%w: split burst cannot hold two maximum transactions for %q", agent.ErrCapacity, name)
	}
	const measuredTransport = 3474 // ADR 0010 C; rechecked from real payloads at runtime.
	legacyBytes := measuredTransport + snapshotEnvelopeBytes + base64.StdEncoding.EncodedLen(bounds.MaxStateBytes) + v2HostMetadataBytes
	if !isSplit && legacyBytes > limits.MaxBytes {
		return fmt.Errorf("%w: declared state for %q needs %d encoded bytes; operator snapshot cap %d is short by %d bytes", agent.ErrStateTooLarge, name, legacyBytes, limits.MaxBytes, legacyBytes-limits.MaxBytes)
	}
	splitBytes := bounds.MaxStateBytes + 6 + componentCipherOverhead
	if isSplit && splitBytes > limits.MaxBytes {
		return fmt.Errorf("%w: declared working component for %q needs %d encoded bytes; operator hot cap %d is short by %d bytes", agent.ErrStateTooLarge, name, splitBytes, limits.MaxBytes, splitBytes-limits.MaxBytes)
	}
	metadata, err := json.Marshal(v2Envelope{Name: name, Contract: v2Contract, TaskKinds: kinds, Limits: &declared, Quota: &snapshotQuota{Version: 1, Balance: quotaBalance(uint64(limits.BurstBytes)), At: time.Now().UTC()}})
	if err != nil {
		return err
	}
	if len(metadata) > bounds.MaxPendingBytes {
		return fmt.Errorf("%w: registration metadata", agent.ErrCapacity)
	}
	return nil
}

// validateRestoredAgentBounds checks the encoded content against this worker,
// independently of identity. Never truncate an adopted state or payload.
func (s *session) validateRestoredAgentBounds(b agenthost.Bounds, exact bool) error {
	a := s.state.Agent
	if len(a.History) > b.MaxHistoryBytes || (!s.componentMode && len(a.History) != 0) {
		return fmt.Errorf("%w: %w", agent.ErrCapacity, agent.ErrHistoryTooLarge)
	}
	if len(a.State.Bytes) > b.MaxStateBytes {
		return fmt.Errorf("%w: %w", agent.ErrCapacity, agent.ErrStateTooLarge)
	}
	metadata, err := json.Marshal(a.V2)
	if err != nil {
		return err
	}
	if len(metadata) > b.MaxPendingBytes {
		return fmt.Errorf("%w: restored host metadata exceeds local limit", agent.ErrCapacity)
	}
	if exact && a.V2.MoveOnlyLength > b.MaxMailboxBytes {
		return fmt.Errorf("%w: restored move data exceeds local limit", agent.ErrCapacity)
	}
	active := 0
	for _, t := range a.V2.Tasks {
		if t.Phase == taskStarted && !t.Final {
			active++
		}
	}
	if exact && active > b.MaxActiveTasks {
		return fmt.Errorf("%w: restored active tasks exceed local limit", agent.ErrCapacity)
	}
	p := a.V2.Playback
	if exact && (p.Length > b.MaxAudioBytes || (b.MaxAudioDuration > 0 && p.Duration > b.MaxAudioDuration)) {
		return fmt.Errorf("%w: restored audio exceeds local limit", agent.ErrCapacity)
	}
	return nil
}

// Include the conservative Redis encryption envelope even with a memory store,
// so a pool has one budget independent of its configured store implementation.
const snapshotEnvelopeBytes = 42

// A refused callback publishes one typed Capacity notice after rollback. It
// never repeatedly invokes an event whose state can never fit. Input progress
// records the explicit refusal, while the prior application state is retained.
var errSnapshotAdmissionCapacity = fmt.Errorf("%w: snapshot admission", agent.ErrCapacity)
