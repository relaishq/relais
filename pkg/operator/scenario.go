package operator

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
}
