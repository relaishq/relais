// Package inventory validates independent host inventories before an effect.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

// Pin is an inventory's identity proof, independent of the session protocol.
type Pin struct {
	Architecture   string `json:"architecture,omitempty"`
	Version        string `json:"version"`
	Commit         string `json:"commit"`
	ArchiveSHA256  string `json:"archiveSha256"`
	ManifestSHA256 string `json:"manifestSha256"`
	BootSHA256     string `json:"bootSha256"`
}

var Slots = []string{"relay", "w1", "w2", "shared"}
var expected = map[string][]string{"relay": {"relay-primary", "relay-standby"}, "w1": {"w1"}, "w2": {"w2"}, "shared": {"control", "signal"}}
var category = regexp.MustCompile(`^INVENTORY_UNKNOWN:([a-z][a-z-]{0,29})\s*$`)
var sha = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Failure struct {
	Slot, Code string
	cause      error
}

func (f *Failure) Error() string { return "inventory-unavailable:" + f.Slot + "-" + f.Code }
func (f *Failure) Unwrap() error { return f.cause }
func Unavailable(slot, stdout string) error {
	code := "unavailable"
	if match := category.FindStringSubmatch(stdout); match != nil {
		code = match[1]
	}
	return &Failure{Slot: slot, Code: code}
}

// ValidatePins binds runtime inventories to the independently selected judges.
func ValidatePins(pins []Pin, selected operator.Scenario) error {
	if len(pins) != 2 || pins[0].Version == pins[1].Version {
		return fmt.Errorf("runtime release pins differ from judges")
	}
	for i, p := range pins {
		identity := []operator.Identity{selected.ReleaseA, selected.ReleaseB}[i]
		if identity.SourceCommit == "" || identity.SourceCommit != p.Commit || identity.SHA256 != p.ArchiveSHA256 || !sha.MatchString(p.ArchiveSHA256) || !sha.MatchString(p.ManifestSHA256) || !sha.MatchString(p.BootSHA256) {
			return fmt.Errorf("runtime release pins differ from judges")
		}
	}
	return nil
}

func Parse(slot string, data []byte, pins []Pin) (participants []releasecompat.Participant, err error) {
	check := "shape"
	defer func() {
		if err != nil {
			err = &Failure{Slot: slot, Code: check, cause: err}
		}
	}()
	var envelope struct {
		ObservedAt   int64             `json:"observedAt"`
		Participants []json.RawMessage `json:"participants"`
	}
	if err := observe.Decode(data, &envelope, observe.Contract{"observedAt", "participants"}); err != nil {
		return nil, err
	}
	names, ok := expected[slot]
	if !ok || len(envelope.Participants) != len(names) || envelope.ObservedAt < -(1<<53-1) || envelope.ObservedAt > 1<<53-1 {
		return nil, fmt.Errorf("incomplete live inventory")
	}
	seen := map[string]bool{}
	rows := make([]releasecompat.Participant, 0, len(names))
	for _, raw := range envelope.Participants {
		check = "shape"
		var p releasecompat.Participant
		if err := observe.Decode(raw, &p, observe.Contract{"name", "role", "profile", "version", "commit", "archive_sha256", "agents", "default_budgets", "manifest"}); err != nil {
			return nil, err
		}
		if err := releasecompat.Decode(raw, &p); err != nil {
			return nil, err
		}
		// Standalone participants are not a public releasecompat decode mode.
		// Apply the existing Manifest decoder to its original nested bytes.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if err := releasecompat.Decode(fields["manifest"], &p.Manifest); err != nil {
			return nil, err
		}
		role := p.Name
		if role == "w1" || role == "w2" {
			role = "worker"
		} else if strings.HasPrefix(role, "relay-") {
			role = "relay"
		}
		profile, agents := "default", []string{}
		if role == "worker" {
			profile, agents = "legacy", []string{"echo"}
		}
		pin := slices.IndexFunc(pins, func(pin Pin) bool {
			return pin.Version == p.Version && pin.Commit == p.Commit && pin.ArchiveSHA256 == p.ArchiveSHA256
		})
		check = "participant"
		if pin < 0 || seen[p.Name] || !slices.Contains(names, p.Name) || p.Role != role || p.Profile != profile || p.DefaultBudgets != (role == "worker") || p.Agents == nil || !slices.Equal(p.Agents, agents) {
			return nil, fmt.Errorf("unproven compatibility participant")
		}
		arch := pins[pin].Architecture
		switch arch {
		case "", "arm64":
			arch = "arm64"
		case "x86_64":
			arch = "amd64"
		default:
			check = "architecture"
			return nil, fmt.Errorf("unsupported pinned architecture")
		}
		check = "manifest-identity"
		if p.Manifest.Version != pins[pin].Version || p.Manifest.Commit != pins[pin].Commit || p.Manifest.OS != "linux" || p.Manifest.Arch != arch {
			return nil, fmt.Errorf("live manifest identity differs")
		}
		seen[p.Name] = true
		rows = append(rows, p)
	}
	return rows, nil
}

// BootHash matches the template's literal block without changing its bytes.
// The caller must first validate the reviewed template identity.
func BootHash(template string) (string, error) {
	const marker = "        /opt/relais/boot/run.sh:\n          content: |\n"
	_, tail, ok := strings.Cut(template, marker)
	if !ok {
		return "", fmt.Errorf("missing reviewed boot script")
	}
	var body []string
	for line := range strings.SplitSeq(tail, "\n") {
		if !strings.HasPrefix(line, "            ") {
			break
		}
		body = append(body, line[12:])
	}
	sum := sha256.Sum256([]byte(strings.Join(body, "\n") + "\n"))
	return hex.EncodeToString(sum[:]), nil
}
