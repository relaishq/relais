package operatorcaller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

const maxSafeInteger = 9007199254740991

const (
	NearRTT         = 10 * time.Millisecond
	ClockBurstCount = 10
	ReportGrace     = 180 * time.Second
	AckReserve      = 5 * time.Second
)

type Phase string

const (
	Survivor          Phase = "survivor"
	AdmissionBoundary Phase = "admission-boundary"
)

type Identity struct {
	RunID     string        `json:"runId"`
	Gate      operator.Gate `json:"gate"`
	Phase     Phase         `json:"phase"`
	CallerID  string        `json:"githubRun"` // Compatibility slot; the launcher allocates it before submission.
	Attempt   string        `json:"attempt"`
	PublicSHA string        `json:"publicSha"`
}

var runPattern = regexp.MustCompile(`^run-[a-z0-9-]{1,40}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var positivePattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
var operationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)

func (i Identity) Validate() error {
	if len(i.Unit()) > 255 || !runPattern.MatchString(i.RunID) || !i.Gate.Valid() || (i.Phase != Survivor && i.Phase != AdmissionBoundary) || !positivePattern.MatchString(i.CallerID) || !positivePattern.MatchString(i.Attempt) || !shaPattern.MatchString(i.PublicSHA) {
		return errors.New("invalid-caller-identity")
	}
	return nil
}
func (i Identity) Root() string {
	return fmt.Sprintf("conductor/%s/preview/%s/%s/%s", i.RunID, i.Gate, i.Phase, i.CallerID)
}
func (i Identity) Job() string {
	return fmt.Sprintf("jobs/%s/%s/%s/caller-1/preview/%s/%s", i.RunID, i.CallerID, i.Attempt, i.Gate, i.Phase)
}
func (i Identity) Unit() string {
	return fmt.Sprintf("relais-caller-%s-%s-%s-%s.service", i.RunID, i.Gate, i.Phase, i.CallerID)
}

type Plan struct {
	Version         int    `json:"version"`
	Phase           Phase  `json:"phase"`
	Jobs            int    `json:"jobs"`
	Calls           int    `json:"calls"`
	WorkerCallCap   int    `json:"workerCallCap"`
	DurationSeconds int    `json:"durationSeconds"`
	PublicSHA       string `json:"publicSha"`
}

func NewPlan(phase Phase, seconds int, sha string) Plan {
	calls := 1
	if phase == AdmissionBoundary {
		calls = 2
	}
	return Plan{1, phase, 1, calls, 2, seconds, sha}
}
func (p Plan) Validate() error {
	if p != NewPlan(p.Phase, p.DurationSeconds, p.PublicSHA) || (p.Phase != Survivor && p.Phase != AdmissionBoundary) || p.DurationSeconds < 1 || p.DurationSeconds > 1200 || !shaPattern.MatchString(p.PublicSHA) {
		return errors.New("invalid-caller-plan")
	}
	return nil
}

type Endpoints struct {
	RunID                  string `json:"runId"`
	Signaling              string `json:"signaling"`
	Relay                  string `json:"relay"`
	Bind                   string `json:"bind,omitempty"`
	TokenFile              string `json:"tokenFile,omitempty"`
	FrontendControlBoundNS int64  `json:"frontendControlBoundNs"`
	FrontendWorkerBoundNS  int64  `json:"frontendWorkerBoundNs"`
	DriftPPM               int    `json:"driftPpm"`
}

func CallerArgs(p Plan, e Endpoints) ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(e.Signaling)
	relay, rerr := netip.ParseAddrPort(e.Relay)
	bind, berr := netip.ParseAddrPort(e.Bind)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "/calls" || u.RawQuery != "" || u.Fragment != "" || rerr != nil || berr != nil || !relay.Addr().Is4() || !bind.Addr().Is4() || !runPattern.MatchString(e.RunID) || e.TokenFile == "" || strings.ContainsAny(e.TokenFile, "\r\n\x00") || e.FrontendControlBoundNS < 0 || e.FrontendWorkerBoundNS < 0 || e.DriftPPM <= 0 || e.FrontendControlBoundNS > maxSafeInteger || e.FrontendWorkerBoundNS > maxSafeInteger || e.DriftPPM > maxSafeInteger {
		return nil, errors.New("invalid-caller-endpoints")
	}
	return []string{"-external-caller", "-distributed", "-run-id=" + e.RunID, fmt.Sprint("-callers=", p.Calls), "-gomaxprocs=2", fmt.Sprint("-duration=", p.DurationSeconds, "s"), "-sample-every=1s", "-video=true", "-caller-bind=" + e.Bind, "-signaling=" + e.Signaling, "-relay=" + e.Relay, "-bearer-token-file=" + e.TokenFile, fmt.Sprint("-frontend-control-bound=", e.FrontendControlBoundNS, "ns"), fmt.Sprint("-frontend-worker-bound=", e.FrontendWorkerBoundNS, "ns"), fmt.Sprint("-clock-drift-ppm=", e.DriftPPM), "-event-retention=180s"}, nil
}

type Manifest struct {
	Version int `json:"version"`
	Identity
	ArmedAt   int64     `json:"armedAt"`
	CreatedAt int64     `json:"createdAt"`
	Deadline  int64     `json:"deadline"`
	Plan      Plan      `json:"plan"`
	Endpoints Endpoints `json:"endpoints"`
}

func (m Manifest) Validate(i Identity, now time.Time) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if err := m.Plan.Validate(); err != nil {
		return err
	}
	if m.Gate == operator.Failover && m.Plan.DurationSeconds < 1020 {
		return errors.New("insufficient-failover-duration")
	}
	if m.Version != 1 || m.Identity != i || m.PublicSHA != m.Plan.PublicSHA || m.Phase != m.Plan.Phase || m.ArmedAt < 0 || m.CreatedAt < m.ArmedAt || m.CreatedAt > now.UnixMilli() || m.Deadline <= now.UnixMilli() || m.Deadline-m.ArmedAt > int64(105*time.Minute/time.Millisecond) || m.Deadline-m.CreatedAt > int64(20*time.Minute/time.Millisecond) || int64(m.Plan.DurationSeconds)*1000+120000 > m.Deadline-now.UnixMilli() || m.Endpoints.RunID != i.RunID {
		return errors.New("invalid-caller-manifest")
	}
	e := m.Endpoints
	e.Bind = "192.0.2.1:0"
	e.TokenFile = "caller.token"
	_, err := CallerArgs(m.Plan, e)
	return err
}

type Host struct {
	SchemaVersion    int       `json:"schemaVersion"`
	AvailableCPUs    int       `json:"availableCpus"`
	LogicalCPUs      int       `json:"logicalCpus"`
	CPUModel         string    `json:"cpuModel"`
	CallerGOMAXPROCS int       `json:"callerGomaxprocs"`
	Arch             string    `json:"arch"`
	Platform         string    `json:"platform"`
	CapturedAt       time.Time `json:"capturedAt"`
}

func (h Host) Validate() error {
	if h.SchemaVersion != 1 || h.AvailableCPUs < 2 || h.AvailableCPUs > maxSafeInteger || h.LogicalCPUs < 2 || h.LogicalCPUs > maxSafeInteger || strings.TrimSpace(h.CPUModel) == "" || h.CallerGOMAXPROCS != 2 || h.CapturedAt.IsZero() {
		return errors.New("invalid-caller-host")
	}
	return nil
}

type Probe struct {
	RTTNS         int64     `json:"rttNs"`
	CallerSend    time.Time `json:"callerSend"`
	CallerReceive time.Time `json:"callerReceive"`
	ServerReceive time.Time `json:"serverReceive"`
	ServerSend    time.Time `json:"serverSend"`
}
type Burst struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Probes     []Probe   `json:"probes"`
}

func (b Burst) Validate() error {
	if observe.Numbers(b) != nil || b.StartedAt.IsZero() || b.FinishedAt.Before(b.StartedAt) || len(b.Probes) < ClockBurstCount || len(b.Probes) > 32 {
		return errors.New("invalid-clock-burst")
	}
	for _, p := range b.Probes {
		if p.RTTNS <= 0 || p.RTTNS > int64(time.Second) || p.CallerSend.IsZero() || p.CallerReceive.IsZero() || p.ServerReceive.IsZero() || p.ServerSend.IsZero() || p.CallerReceive.Before(p.CallerSend) || p.ServerSend.Before(p.ServerReceive) || p.CallerSend.Before(b.StartedAt) || p.CallerReceive.After(b.FinishedAt) {
			return errors.New("invalid-clock-probe")
		}
	}
	return nil
}

type Registration struct {
	Version      int      `json:"version"`
	RunID        string   `json:"runId"`
	Phase        Phase    `json:"phase"`
	PublicSHA    string   `json:"publicSha"`
	Sessions     []string `json:"sessions"`
	RegisteredAt int64    `json:"registeredAt"`
	Burst        Burst    `json:"burst"`
	Near         bool     `json:"near"`
	Host         Host     `json:"host"`
}

func Register(p Plan, run string, sessions []string, b Burst, h Host, at time.Time, firstFault *time.Time) (Registration, error) {
	if err := p.Validate(); err != nil {
		return Registration{}, err
	}
	if err := b.Validate(); err != nil {
		return Registration{}, err
	}
	if err := h.Validate(); err != nil {
		return Registration{}, err
	}
	seen := map[string]bool{}
	near := false
	for _, s := range sessions {
		if !sessionPattern.MatchString(s) || seen[s] {
			return Registration{}, errors.New("invalid-registration-sessions")
		}
		seen[s] = true
	}
	for _, probe := range b.Probes {
		near = near || probe.RTTNS <= int64(NearRTT)
	}
	if !runPattern.MatchString(run) || len(sessions) != p.Calls || at.UnixMilli() < 0 || b.FinishedAt.UnixMilli() > at.UnixMilli() || h.CapturedAt.UnixMilli() > at.UnixMilli() || firstFault != nil && at.UnixMilli() >= firstFault.UnixMilli() {
		return Registration{}, errors.New("invalid-registration-time")
	}
	b.Probes = append([]Probe{}, b.Probes...)
	return Registration{1, run, p.Phase, p.PublicSHA, append([]string{}, sessions...), at.UnixMilli(), b, near, h}, nil
}

type Receipt struct {
	Identity      callharness.OperationIdentity `json:"identity"`
	Kind          string                        `json:"kind"`
	Start         time.Time                     `json:"start"`
	End           time.Time                     `json:"end"`
	Result        controlplane.MoveResult       `json:"result"`
	Uncertain     bool                          `json:"uncertain"`
	Codes         []string                      `json:"codes,omitempty"`
	HTTPStatus    int                           `json:"httpStatus,omitempty"`
	ResponseError string                        `json:"responseError,omitempty"`
}
type Payload struct {
	Kind       string                          `json:"kind"`
	Deadline   time.Time                       `json:"deadline,omitzero"`
	Count      int                             `json:"count,omitempty"`
	At         time.Time                       `json:"at,omitzero"`
	Identities []callharness.OperationIdentity `json:"identities,omitempty"`
	Receipts   []Receipt                       `json:"receipts,omitempty"`
	Reason     string                          `json:"reason,omitempty"`
}
type Feed struct {
	SchemaVersion int     `json:"schemaVersion"`
	RunID         string  `json:"runId"`
	Sequence      int     `json:"sequence"`
	OperationID   string  `json:"operationId"`
	Payload       Payload `json:"payload"`
}
type Ack struct {
	RunID       string                          `json:"runId"`
	Job         string                          `json:"job"`
	Sequence    int                             `json:"sequence"`
	OperationID string                          `json:"operationId"`
	OK          bool                            `json:"ok"`
	Error       string                          `json:"error,omitempty"`
	Identities  []callharness.OperationIdentity `json:"identities,omitempty"`
	ClockProbes json.RawMessage                 `json:"clockProbes,omitempty"`
}

func createJSON(ctx context.Context, m Store, key string, value any) CreateState {
	b, err := json.Marshal(value)
	if err != nil {
		return CreateUnknown
	}
	return m.Create(ctx, key, b)
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func decode(raw []byte, value any) error {
	return observe.DecodeLimit(raw, value, nil, operator.MaxBlobBytes)
}
