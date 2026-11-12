package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Gate order is part of the public scenario, not an adapter decision.
type Gate string

const (
	Rollout         Gate = "rollout"
	ControlRestart  Gate = "control-restart"
	UpgradeRollback Gate = "upgrade-rollback"
	Failover        Gate = "failover"
	RelayDeploy     Gate = "relay-deploy"
	Load            Gate = "load"
)

func (g Gate) Valid() bool {
	switch g {
	case Rollout, ControlRestart, UpgradeRollback, Failover, RelayDeploy, Load:
		return true
	}
	return false
}

// Identity separates source from the bytes executed. Platform builds can share
// SourceCommit without sharing SHA256.
type Identity struct {
	SourceCommit string `json:"sourceCommit"`
	SHA256       string `json:"sha256"`
}

type Scenario struct {
	SchemaVersion int      `json:"schemaVersion"`
	Digest        string   `json:"digest"`
	Gates         []Gate   `json:"gates"`
	Template      Identity `json:"template"`
	ReleaseA      Identity `json:"releaseA"`
	ReleaseB      Identity `json:"releaseB"`
	Judge         Identity `json:"judge"`
	Caller        Identity `json:"caller"`
	// Omitted until receipt binding is activated; existing scenario bytes stay identical.
	ClockReceiptSHA256 string `json:"clockReceiptSha256,omitempty"`
}

// ScenarioIdentityBytes returns the exact digest preimage. The digest itself
// is excluded; an empty receipt preserves the existing unbound encoding.
func ScenarioIdentityBytes(s Scenario) ([]byte, error) {
	if s.ClockReceiptSHA256 != "" {
		decoded, err := hex.DecodeString(s.ClockReceiptSHA256)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != s.ClockReceiptSHA256 {
			return nil, fmt.Errorf("invalid clock receipt digest")
		}
	}
	s.Digest = ""
	return json.Marshal(s)
}

// BuildScenarioIdentity is a pure builder. Activation must supply the digest
// returned by clocks.BindReceipt, rather than an artifact's claimed digest.
func BuildScenarioIdentity(s Scenario, receiptSHA256 string) (Scenario, error) {
	s.ClockReceiptSHA256 = receiptSHA256
	raw, err := ScenarioIdentityBytes(s)
	if err != nil {
		return Scenario{}, err
	}
	sum := sha256.Sum256(raw)
	s.Digest = hex.EncodeToString(sum[:])
	s.Gates = slices.Clone(s.Gates)
	return s, nil
}

// PlanIdentity is the scenario digest with no clock receipt. Every input is
// known before a host exists, so readiness can check it before deployment.
func PlanIdentity(s Scenario) (string, error) {
	plan, err := BuildScenarioIdentity(s, "")
	return plan.Digest, err
}

// RuntimeBinding joins one plan to one measured clock receipt. Scenario is the
// plan with the receipt added; its Digest is the runtime identity Init carries.
type RuntimeBinding struct {
	PlanIdentity string   `json:"planIdentity"`
	Scenario     Scenario `json:"scenario"`
}

// ErrPlanIdentity refuses a scenario that is not the plan its digest names.
var ErrPlanIdentity = errors.New("clock-binding-digest-differ")

// CheckPlan returns the plan identity of an unbound scenario whose digest is
// that identity. A scenario that already carries a receipt, or that changed
// after its digest was fixed, is not a plan and must not be bound.
func CheckPlan(s Scenario) (string, error) {
	plan, err := PlanIdentity(s)
	if err != nil || s.ClockReceiptSHA256 != "" || s.Digest != plan {
		return "", ErrPlanIdentity
	}
	return plan, nil
}

// BindRuntime binds only a checked plan and never changes its identity: a
// different receipt gives a different Scenario.Digest under the same PlanIdentity.
func BindRuntime(s Scenario, receiptSHA256 string) (RuntimeBinding, error) {
	if receiptSHA256 == "" {
		return RuntimeBinding{}, fmt.Errorf("invalid clock receipt digest")
	}
	plan, err := CheckPlan(s)
	if err != nil {
		return RuntimeBinding{}, err
	}
	bound, err := BuildScenarioIdentity(s, receiptSHA256)
	if err != nil {
		return RuntimeBinding{}, err
	}
	return RuntimeBinding{PlanIdentity: plan, Scenario: bound}, nil
}
