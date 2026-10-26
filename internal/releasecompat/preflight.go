package releasecompat

import (
	"fmt"
	"slices"
	"sort"
)

type Participant struct {
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	Profile        string   `json:"profile"`
	Version        string   `json:"version"`
	Commit         string   `json:"commit"`
	ArchiveSHA256  string   `json:"archive_sha256"`
	Agents         []string `json:"agents"`
	DefaultBudgets bool     `json:"default_budgets"`
	Manifest       Manifest `json:"manifest"`
}
type Pool struct {
	Schema              int           `json:"inventory_version"`
	Complete            bool          `json:"complete"`
	TargetWorkerProfile string        `json:"target_worker_profile"`
	Participants        []Participant `json:"participants"`
}
type Step struct {
	Participant string `json:"participant"`
	Role        string `json:"role"`
	Profile     string `json:"profile"`
	Version     string `json:"version"`
}
type Verdict struct {
	Allowed bool     `json:"allowed"`
	Refusal *Refusal `json:"refusal,omitempty"`
	Steps   []Step   `json:"steps,omitempty"`
}

// Preflight simulates workers first, then relay (including standby), control and
// signaling. Steps are a reviewed plan, never executable process instructions.
func Preflight(pool Pool, target Manifest, operation string) Verdict {
	fail := func(err error) Verdict {
		r, ok := err.(*Refusal)
		if !ok {
			r = refuse("invalid_input", err.Error())
		}
		return Verdict{Refusal: r}
	}
	if operation != "upgrade" && operation != "rollback" {
		return fail(refuse("invalid_operation", "expected upgrade or rollback"))
	}
	if err := target.Validate(); err != nil {
		return fail(err)
	}
	if target.Commit == "" || target.OS == "any" || target.Arch == "any" || developmentVersion(target.Version) {
		return fail(refuse("unknown_build", "target must be a stamped release archive manifest with a real release version"))
	}
	if pool.Schema != 1 || !pool.Complete || len(pool.Participants) == 0 || len(pool.Participants) > 256 {
		return fail(refuse("unknown_inventory", "complete bounded inventory is required"))
	}
	if pool.TargetWorkerProfile != "legacy" && pool.TargetWorkerProfile != "split" && pool.TargetWorkerProfile != "bridge" {
		return fail(refuse("unknown_profile", "target_worker_profile must be legacy, split or bridge"))
	}
	current := slices.Clone(pool.Participants)
	names, roles := map[string]bool{}, map[string]int{}
	// Formats ever supported for continued state are conservatively retained. An
	// admission flag never proves that older format state disappeared from Redis.
	retained := map[string][]int{}
	for _, p := range current {
		if p.Name == "" || names[p.Name] || !digestPattern.MatchString(p.ArchiveSHA256) {
			return fail(refuse("unknown_inventory", "unique names and verified archive SHA-256 are required"))
		}
		names[p.Name] = true
		if err := p.Manifest.Validate(); err != nil {
			return fail(err)
		}
		if developmentVersion(p.Version) {
			return fail(refuse("unknown_build", "development artifacts are not supported running releases: "+p.Name))
		}
		if p.Version != p.Manifest.Version || p.Commit == "" || p.Commit != p.Manifest.Commit {
			return fail(refuse("identity_mismatch", "observed binary identity must match installed manifest: "+p.Name))
		}
		if p.Manifest.OS != target.OS || p.Manifest.Arch != target.Arch {
			return fail(refuse("platform_mismatch", "target archive platform differs from "+p.Name))
		}
		if p.Role != "worker" && p.Role != "relay" && p.Role != "control" && p.Role != "signal" {
			return fail(refuse("unknown_role", p.Role))
		}
		roles[p.Role]++
		profile, ok := p.Manifest.Roles[p.Role][p.Profile]
		if !ok {
			return fail(refuse("unknown_profile", p.Name))
		}
		if p.Role == "worker" && (p.Profile != "legacy" && p.Profile != "split" && p.Profile != "bridge" || len(p.Agents) == 0) {
			return fail(refuse("unknown_agent", "worker requires an explicit profile and agent inventory"))
		}
		if p.Role == "worker" && slices.Contains(p.Agents, "bridge") && (p.Profile != "bridge" || pool.TargetWorkerProfile != "bridge") {
			return fail(refuse("unknown_profile", "bridge agents require the bridge-enabled split profile"))
		}
		if p.Role == "worker" && !p.DefaultBudgets {
			return fail(refuse("unknown_configuration", "worker profile requires default state budgets; custom overrides need separate compatibility evidence: "+p.Name))
		}
		for _, name := range p.Agents {
			if _, ok := findAgent(p.Manifest, name); !ok {
				return fail(refuse("unknown_agent", p.Name+": "+name))
			}
		}
		c, err := Compare(target.Version, p.Version)
		if err != nil {
			return fail(err)
		}
		if operation == "upgrade" && c < 0 || operation == "rollback" && c > 0 {
			return fail(refuse("release_direction", "target does not match requested operation"))
		}
		if c == 0 && (p.Manifest.Version != target.Version || p.Manifest.Commit != target.Commit) {
			return fail(refuse("identity_mismatch", "a release version must identify one immutable build"))
		}
		if operation == "rollback" {
			c, err = Compare(target.Version, p.Manifest.OldestRollback)
			if err != nil {
				return fail(err)
			}
			if c < 0 {
				return fail(refuse("rollback_boundary", "target is older than the running release rollback boundary"))
			}
		}
		for domain, f := range profile.Formats {
			if stateDomain(domain) && (!bridgeDomain(domain) || (p.Role == "worker" && slices.Contains(p.Agents, "bridge"))) {
				retained[domain] = union(retained[domain], f.Continues)
			}
		}
	}
	for _, role := range []string{"worker", "relay", "control", "signal"} {
		if roles[role] == 0 {
			return fail(refuse("unknown_inventory", "missing "+role+" participant"))
		}
	}
	if err := checkState(current, retained); err != nil {
		return fail(err)
	}
	rank := map[string]int{"worker": 0, "relay": 1, "control": 2, "signal": 3}
	sort.SliceStable(current, func(i, j int) bool {
		if operation == "rollback" {
			return rank[current[i].Role] > rank[current[j].Role]
		}
		return rank[current[i].Role] < rank[current[j].Role]
	})
	var steps []Step
	for i, p := range current {
		profileName := "default"
		if p.Role == "worker" {
			profileName = pool.TargetWorkerProfile
		}
		profile, ok := target.Roles[p.Role][profileName]
		if !ok {
			return fail(refuse("unknown_profile", "target lacks "+p.Role+"/"+profileName))
		}
		for _, a := range p.Agents {
			from, _ := findAgent(p.Manifest, a)
			to, ok := findAgent(target, a)
			if !ok || from.Identity != to.Identity || from.Limits != to.Limits {
				return fail(refuse("agent_incompatible", "target registration or declared limits differ: "+a))
			}
			for _, v := range union32(from.Reads, from.Writes) {
				if !slices.Contains(to.Reads, v) || !slices.Contains(to.Writes, v) {
					return fail(refuse("agent_schema", "target cannot restore and continue existing app schema: "+a))
				}
			}
		}
		current[i].Manifest = target
		current[i].Profile = profileName
		current[i].Version = target.Version
		current[i].Commit = target.Commit
		for domain, f := range profile.Formats {
			if stateDomain(domain) {
				retained[domain] = union(retained[domain], f.Writes)
			}
		}
		if err := checkState(current, retained); err != nil {
			if r, ok := err.(*Refusal); ok {
				r.Step = i + 1
			}
			return fail(err)
		}
		steps = append(steps, Step{Participant: p.Name, Role: p.Role, Profile: profileName, Version: target.Version})
	}
	return Verdict{Allowed: true, Steps: steps}
}
func stateDomain(s string) bool {
	return bridgeDomain(s) || s == "redis_keys" || s == "snapshot" || s == "component" || s == "metadata" || s == "encryption_header" || s == "legacy_encryption_header"
}
func union(a, b []int) []int {
	r := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(r, v) {
			r = append(r, v)
		}
	}
	slices.Sort(r)
	return r
}
func union32(a, b []uint32) []uint32 {
	r := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(r, v) {
			r = append(r, v)
		}
	}
	return r
}
func findAgent(m Manifest, name string) (Agent, bool) {
	for _, a := range m.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}
func checkState(participants []Participant, retained map[string][]int) error {
	for i, a := range participants {
		for _, b := range participants[i+1:] {
			higher, lower := a.Manifest, b.Manifest
			c, err := Compare(higher.Version, lower.Version)
			if err != nil {
				return err
			}
			if c < 0 {
				higher, lower = lower, higher
			}
			if higher.Version == lower.Version {
				if higher.Commit != lower.Commit {
					return refuse("identity_mismatch", "mixed builds reuse a release version")
				}
				continue
			}
			c, err = Compare(lower.Version, higher.OldestLivePeer)
			if err != nil {
				return err
			}
			if c < 0 || !slices.Contains(higher.TestedPeers, lower.Version) {
				return refuse("unsupported_pair", "release pair is outside the tested live compatibility policy: "+higher.Version+" / "+lower.Version)
			}
		}
	}
	needed := map[string][]int{}
	for domain, versions := range retained {
		needed[domain] = slices.Clone(versions)
	}
	for _, p := range participants {
		for domain, f := range p.Manifest.Roles[p.Role][p.Profile].Formats {
			needed[domain] = union(needed[domain], f.Writes)
		}
	}
	for _, p := range participants {
		formats := p.Manifest.Roles[p.Role][p.Profile].Formats
		for domain, versions := range needed {
			f, ok := formats[domain]
			if !ok && (!bridgeDomain(domain) || (p.Role != "worker" && p.Role != "control")) {
				continue
			}
			for _, v := range versions {
				if !slices.Contains(f.Reads, v) || !slices.Contains(f.Continues, v) {
					r := refuse("format_incompatible", fmt.Sprintf("%s cannot read and continue %s format %d", p.Name, domain, v))
					r.Participant, r.Format = p.Name, domain
					return r
				}
			}
		}
	}
	// Agents can move between any workers. Check both app directions in every
	// intermediate state, independently of the platform snapshot envelope.
	for _, a := range participants {
		if a.Role != "worker" {
			continue
		}
		for _, name := range a.Agents {
			app, _ := findAgent(a.Manifest, name)
			for _, b := range participants {
				if b.Role != "worker" {
					continue
				}
				peer, ok := findAgent(b.Manifest, name)
				if !ok || !slices.Contains(b.Agents, name) || app.Identity != peer.Identity || app.Limits != peer.Limits {
					return refuse("agent_incompatible", "mixed worker registrations differ: "+name)
				}
				for _, v := range app.Writes {
					if !slices.Contains(peer.Reads, v) || !slices.Contains(peer.Writes, v) {
						return refuse("agent_schema", "mixed workers cannot continue app schema: "+name)
					}
				}
			}
		}
	}
	return nil
}

func bridgeDomain(s string) bool { return s == "bridge_output" || s == "bridge_output_move" }
