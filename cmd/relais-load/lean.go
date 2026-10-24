package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/netip"
	"net/url"
	"os"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

// leanManifest is an evidence contract with the private acceptance runner.
// It contains no credentials, remote commands, or provider clients.
type leanManifest struct {
	Version            int                `json:"version"`
	RunID              string             `json:"run_id"`
	Signaling          string             `json:"signaling"`
	Relay              string             `json:"relay"`
	Region             string             `json:"region"`
	Build              string             `json:"build"`
	Agent              string             `json:"agent"`
	Payload            string             `json:"payload"`
	StoreDurability    string             `json:"store_durability"`
	StoreTLS           bool               `json:"store_tls"`
	RelayBufferEnabled *bool              `json:"relay_buffer_enabled"`
	Clock              *leanClock         `json:"clock"`
	Callers            []leanCaller       `json:"callers"`
	Phases             []leanPhase        `json:"phases"`
	Cleanup            loadgen.LeakReport `json:"cleanup"`
	ENARequired        bool               `json:"ena_required"`
	Topology           string             `json:"topology,omitempty"` // Empty defaults to cloud; only local waives ENA.
	Prices             *leanPrices        `json:"prices,omitempty"`
}
type leanClock struct {
	ControlBound time.Duration `json:"frontend_control_bound_ns"`
	WorkerBound  time.Duration `json:"frontend_worker_bound_ns"`
	DriftPPM     float64       `json:"drift_ppm"`
	Evidence     string        `json:"independent_measurements"`
}
type leanCaller struct {
	ID     string `json:"id"`
	Runner string `json:"runner"`
	Region string `json:"region"`
	Bind   string `json:"bind"`
}
type leanWorker struct {
	Name         string   `json:"name"`
	Generation   string   `json:"generation"`
	Ready        bool     `json:"ready"`
	Sessions     []string `json:"sessions"`
	Reservations int      `json:"reservations"`
}
type leanPool struct {
	At         time.Time    `json:"at"`
	AgeSeconds float64      `json:"age_seconds"`
	Stale      bool         `json:"stale"`
	Cap        int          `json:"worker_call_cap"`
	Workers    []leanWorker `json:"workers"`
}
type leanTiming struct {
	Identity          callharness.OperationIdentity `json:"identity"`
	ArmAcknowledgedAt time.Time                     `json:"caller_arm_acknowledged_at"`
	ReceiptSentAt     time.Time                     `json:"caller_receipt_sent_at"`
}
type leanEmpty struct {
	Worker        string    `json:"worker"`
	Generation    string    `json:"generation"`
	At            time.Time `json:"at"`
	LocalSessions int       `json:"local_sessions"`
	Owners        int       `json:"owners"`
	Reservations  int       `json:"reservations"`
	Transfers     int       `json:"transfers"`
}
type leanAdmission struct {
	Status         int      `json:"status"`
	Code           string   `json:"code"`
	RetryAfter     string   `json:"retry_after"`
	Before         leanPool `json:"before"`
	After          leanPool `json:"after"`
	RefusalsBefore uint64   `json:"refusals_before"`
	RefusalsAfter  uint64   `json:"refusals_after"`
}
type leanAllowance struct {
	Host               string             `json:"host"`
	Generation         string             `json:"generation"`
	First              map[string]float64 `json:"first"`
	Last               map[string]float64 `json:"last"`
	ConntrackRemaining *float64           `json:"conntrack_remaining"`
}
type leanPhase struct {
	CompletedAt time.Time       `json:"completed_at"` // Control-clock observation after callers close.
	ID          string          `json:"id"`
	Kind        string          `json:"kind"` // move, kill, drain, boundary
	CallerIDs   []string        `json:"caller_ids"`
	Before      leanPool        `json:"before"`
	After       leanPool        `json:"after"`
	Restored    leanPool        `json:"restored"`
	DrainEmpty  *leanEmpty      `json:"drain_empty,omitempty"`
	Timings     []leanTiming    `json:"timings"`
	Admission   *leanAdmission  `json:"admission,omitempty"`
	Allowances  []leanAllowance `json:"allowances,omitempty"`
	// WindowError bounds the alignment of service scrapes to this media window.
	WindowError    time.Duration     `json:"telemetry_window_error_ns"`
	WindowEvidence string            `json:"telemetry_window_evidence"`
	Run            loadgen.RunReport `json:"run"`
}
type leanSchedule struct {
	FaultTrialSeconds   int    `json:"fault_trial_media_seconds"`
	Moves               int    `json:"moves"`
	Takeovers           int    `json:"takeovers"`
	Drains              int    `json:"drains"`
	TrialSeconds        int    `json:"trial_media_seconds"`
	RestoreSeconds      int    `json:"restore_timeout_seconds"`
	AdmissionSeconds    int    `json:"admission_seconds"`
	SetupCleanupSeconds int    `json:"setup_cleanup_seconds"`
	ExpectedSeconds     int    `json:"expected_wall_seconds"`
	GateWindowSeconds   int    `json:"private_gate_window_seconds"`
	Note                string `json:"note"`
}

func leanPlan() leanSchedule {
	// Each independent 30s trial has >=3s warmup and >7s recovery tail.
	// Ninety seconds is an explicit runner timeout, not a measured boot tail.
	return leanSchedule{
		Moves: 20, Takeovers: 10, Drains: 2, TrialSeconds: 30, FaultTrialSeconds: 120, RestoreSeconds: 90,
		AdmissionSeconds: 120, SetupCleanupSeconds: 240, ExpectedSeconds: 2400, GateWindowSeconds: 7200,
		Note: "40-minute budget: 20 x 30s move media + 12 x 120s live kill/drain media (including up to 90s restore/rejoin) + 120s boundary + 240s setup/cleanup. Run serially at T=1; boundary uses four calls without faults. Includes 20 moves plus two additional drains. Restore deadline expiry is inconclusive; never shorten trial counts. This is an input budget, not measured cloud time or full-soak acceptance.",
	}
}

type leanVerdict struct {
	Outcome string   `json:"outcome"`
	Causes  []string `json:"causes,omitempty"`
}

func (v *leanVerdict) unknown(c string) {
	if v.Outcome != "fail" {
		v.Outcome = "inconclusive"
	}
	v.Causes = append(v.Causes, c)
}
func (v *leanVerdict) fail(c string) { v.Outcome = "fail"; v.Causes = append(v.Causes, c) }
func (v *leanVerdict) merge(prefix string, o leanVerdict) {
	for _, c := range o.Causes {
		if o.Outcome == "fail" {
			v.fail(prefix + c)
		} else {
			v.unknown(prefix + c)
		}
	}
}

type leanAges struct {
	Samples         int     `json:"samples"`
	CheckpointP99MS float64 `json:"checkpoint_age_p99_ms"`
	SnapshotP99MS   float64 `json:"snapshot_age_p99_ms"`
	CheckpointMaxMS float64 `json:"checkpoint_age_max_ms"`
	SnapshotMaxMS   float64 `json:"snapshot_age_max_ms"`
}
type leanPhaseResult struct {
	CommandsLowerPerCallSecond float64  `json:"store_commands_lower_per_call_second"`
	BytesLowerPerCallSecond    float64  `json:"store_bytes_lower_per_call_second"`
	Ages                       leanAges `json:"ages"`
	ID                         string   `json:"id"`
	leanVerdict
	CommandsPerCallSecond    float64         `json:"store_commands_per_call_second"`
	BytesPerCallSecond       float64         `json:"store_bytes_per_call_second"`
	PublicBytesPerCallSecond float64         `json:"public_bytes_per_call_second"`
	Memory                   []memoryVerdict `json:"memory"`
	MemoryGate               leanVerdict     `json:"memory_gate"`
}
type leanReport struct {
	ObservedWallSeconds float64 `json:"observed_wall_seconds"`
	Policy              string  `json:"policy"`
	RunID               string  `json:"run_id"`
	leanVerdict
	SurvivorSafeCalls  int                             `json:"survivor_safe_calls"`
	FullSoakAcceptance bool                            `json:"full_soak_acceptance"`
	Schedule           leanSchedule                    `json:"schedule"`
	Phases             []leanPhaseResult               `json:"phases"`
	Trials             *loadgen.DistributedTrialResult `json:"distributed_trials"`
	Cost               leanCost                        `json:"cost"`
	CloudQualification leanVerdict                     `json:"cloud_qualification"`
	Evidence           leanManifest                    `json:"evidence"`
}

func finiteNonnegative(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }
func readLeanJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, (64<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 64<<20 {
		return errors.New("lean artifact exceeds 64 MiB")
	}
	if err := rejectLeanDuplicateKeys(json.NewDecoder(bytes.NewReader(b))); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("lean artifact must contain one JSON object")
	}
	return nil
}

// Token inspection precedes typed decoding so duplicate fields cannot silently
// replace earlier evidence, including inside reports and escaped key names.
func rejectLeanDuplicateKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if keys[name] {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			keys[name] = true
			if err := rejectLeanDuplicateKeys(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err := rejectLeanDuplicateKeys(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

func validateLeanManifest(m leanManifest) error {
	if m.Topology != "" && m.Topology != "cloud" && m.Topology != "local" {
		return errors.New("lean topology must be cloud or local (omitted means cloud)")
	}
	if m.Version != 1 || m.RunID == "" || m.Region == "" || m.Build == "" || m.Agent == "" || m.Payload == "" || m.StoreDurability != "sync" || !m.StoreTLS || m.RelayBufferEnabled == nil {
		return errors.New("lean requires v1 identity, region/build/agent/payload, sync TLS store and observed relay buffer setting")
	}
	u, err := url.Parse(m.Signaling)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("lean signaling must be credential-free HTTPS")
	}
	a, err := netip.ParseAddrPort(m.Relay)
	if err != nil || !a.Addr().Is4() || a.Addr().IsUnspecified() || a.Port() == 0 {
		return errors.New("lean relay requires a specific IPv4 address:port")
	}
	if m.Clock == nil || m.Clock.ControlBound < 0 || m.Clock.WorkerBound < 0 || !finiteNonnegative(m.Clock.DriftPPM) || m.Clock.DriftPPM >= 1e6 || m.Clock.Evidence == "" {
		return errors.New("lean requires independent measured host clock bounds and drift")
	}
	if len(m.Callers) < 1 || len(m.Callers) > 4 || len(m.Phases) < 1 || len(m.Phases) > 128 {
		return errors.New("lean requires 1..4 caller placements and 1..128 phases")
	}
	ids := map[string]bool{}
	for _, c := range m.Callers {
		a, e := netip.ParseAddrPort(c.Bind)
		if c.ID == "" || ids[c.ID] || c.Runner == "" || c.Region == "" || e != nil || !a.Addr().Is4() || a.Addr().IsUnspecified() || a.Port() != 0 {
			return errors.New("lean caller placements need unique IDs, runner/region and specific IPv4 bind with port 0")
		}
		ids[c.ID] = true
	}
	phases := map[string]bool{}
	for _, p := range m.Phases {
		if p.ID == "" || phases[p.ID] || (p.Kind != "move" && p.Kind != "kill" && p.Kind != "drain" && p.Kind != "boundary") {
			return errors.New("lean needs unique phase IDs and move/kill/drain/boundary kinds")
		}
		phases[p.ID] = true
		if p.Run.Limits != loadgen.DefaultLimits() || p.Run.Duration < 20*time.Second || p.Run.Duration > 3*time.Minute || p.WindowError < 0 || p.WindowError >= p.Run.Duration/2 || p.WindowEvidence == "" {
			return errors.New("lean requires unchanged generator limits, 20s..3m phases and independently bounded telemetry windows shorter than half the phase")
		}
		seen := map[string]bool{}
		for _, id := range p.CallerIDs {
			if !ids[id] || seen[id] {
				return errors.New("unknown or duplicate lean caller placement")
			}
			seen[id] = true
		}
		if len(seen) != 1 || len(p.Run.Processes) != 1 {
			return errors.New("lean requires one external generator per phase (one call, or four at the boundary)")
		}
	}
	return nil
}
func runLean(fs *flag.FlagSet, manifest, output, prices string, plan bool) error {
	if flagWasSet(fs, "lean-plan") && !plan {
		return errors.New("lean-plan requires true")
	}
	var unsupported string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "lean-capacity", "lean-plan", "output", "prices":
		default:
			unsupported = f.Name
		}
	})
	if unsupported != "" {
		return fmt.Errorf("-%s is not supported by the separate lean policy", unsupported)
	}
	if plan {
		if manifest != "" || prices != "" {
			return errors.New("lean-plan cannot judge inputs")
		}
		if output != "" {
			return writeJSON(output, leanPlan())
		}
		return json.NewEncoder(os.Stdout).Encode(leanPlan())
	}
	if output == "" {
		return errors.New("lean-capacity needs -output")
	}
	var m leanManifest
	if err := readLeanJSON(manifest, &m); err != nil {
		return err
	}
	if err := validateLeanManifest(m); err != nil {
		return err
	}
	if prices != "" {
		if m.Prices != nil {
			return errors.New("supply lean prices in either manifest or -prices")
		}
		m.Prices = &leanPrices{}
		if err := readLeanJSON(prices, m.Prices); err != nil {
			return err
		}
	}
	r := judgeLean(m)
	if err := writeJSON(output, r); err != nil {
		return err
	}
	fmt.Printf("lean outcome=%s T=1 expected-wall=%ds cost=%s report=%s\n", r.Outcome, r.Schedule.ExpectedSeconds, r.Cost.Status, output)
	if r.Outcome != "pass" {
		return fmt.Errorf("lean capacity %s; see report", r.Outcome)
	}
	return nil
}

// Explicit presence matters: omitted clock bounds must not imply agreement.
func (c *leanClock) UnmarshalJSON(data []byte) error {
	type plain leanClock
	value := plain{ControlBound: -1, WorkerBound: -1, DriftPPM: -1}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*c = leanClock(value)
	return nil
}
