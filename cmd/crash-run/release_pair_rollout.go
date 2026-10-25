package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/releasecompat"
)

// pairParticipant mirrors the operator inventory in deploy/aws/README.md. The
// manifest is the installed archive's raw compatibility.json.
type pairParticipant struct {
	Name           string          `json:"name"`
	Role           string          `json:"role"`
	Profile        string          `json:"profile"`
	Version        string          `json:"version"`
	Commit         string          `json:"commit"`
	ArchiveSHA256  string          `json:"archive_sha256"`
	Agents         []string        `json:"agents"`
	DefaultBudgets bool            `json:"default_budgets"`
	Manifest       json.RawMessage `json:"manifest"`
}

type pairPool struct {
	Schema              int               `json:"inventory_version"`
	Complete            bool              `json:"complete"`
	TargetWorkerProfile string            `json:"target_worker_profile"`
	Participants        []pairParticipant `json:"participants"`
}

// The lean template runs stock echo workers with default budgets and split
// admission off; other roles have the default profile and no agents.
func newPairParticipant(name, role, version, commit string, release *pairRelease) pairParticipant {
	p := pairParticipant{Name: name, Role: role, Profile: "default", Version: version, Commit: commit, ArchiveSHA256: release.SHA256, Agents: []string{}, Manifest: release.Manifest}
	if role == "worker" {
		p.Profile, p.Agents, p.DefaultBudgets = "legacy", []string{"echo"}, true
	}
	return p
}

// binaryIdentity reads "<name> version=V commit=C date=D" from an installed
// binary, the observed identity the inventory requires.
func readBinaryIdentity(ctx context.Context, binary string) (version, commit string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-version")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("%s -version: %w", binary, err)
	}
	return parseBinaryIdentity(string(out))
}

func parseBinaryIdentity(line string) (version, commit string, err error) {
	for _, field := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(field, "version="); ok {
			version = v
		}
		if c, ok := strings.CutPrefix(field, "commit="); ok {
			commit = c
		}
	}
	if version == "" || commit == "" {
		return "", "", fmt.Errorf("binary identity %q lacks version or commit", strings.TrimSpace(line))
	}
	return version, commit, nil
}

// runPairPreflight runs the target archive's own CLI. A typed refusal is a
// verdict, not an error; an unreadable or inconsistent reply is an error.
func runPairPreflight(ctx context.Context, compat, manifest, poolPath, operation string) (releasecompat.Verdict, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, compat, "preflight", "--running", poolPath, "--target", manifest, "--operation", operation)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var verdict releasecompat.Verdict
	if err := json.Unmarshal(stdout.Bytes(), &verdict); err != nil {
		return verdict, stdout.Bytes(), fmt.Errorf("preflight reply unreadable (%v): %w: %s", runErr, err, strings.TrimSpace(stderr.String()))
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil && verdict.Allowed && verdict.Refusal == nil:
		return verdict, stdout.Bytes(), nil
	case errors.As(runErr, &exit) && !verdict.Allowed && verdict.Refusal != nil && verdict.Refusal.Code != "":
		return verdict, stdout.Bytes(), nil
	default:
		return verdict, stdout.Bytes(), fmt.Errorf("preflight exit %v disagrees with verdict allowed=%t", runErr, verdict.Allowed)
	}
}

func writePairPool(name string, pool pairPool) error {
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(name, data, 0600)
}

// pairRefusal stops a rollout before the step it would have authorized.
type pairRefusal struct {
	Operation string
	Check     int
	Refusal   releasecompat.Refusal
}

func (e *pairRefusal) Error() string {
	return fmt.Sprintf("%s preflight %d refused before mutation: %s: %s", e.Operation, e.Check, e.Refusal.Code, e.Refusal.Reason)
}

// pairActions is the live topology seen by the rollout loop.
type pairActions interface {
	Inventory(ctx context.Context) (pairPool, error)
	Preflight(ctx context.Context, target *pairRelease, pool pairPool, operation string) (releasecompat.Verdict, error)
	// StandbyMissing reports a relay takeover that has not been replenished.
	StandbyMissing() bool
	ReplenishStandby(ctx context.Context, target *pairRelease) error
	Apply(ctx context.Context, step releasecompat.Step, target *pairRelease) error
}

type pairCheck struct {
	Operation, Participant, Role, Action string
	Check                                int
	Preflight, Duration                  time.Duration
}

// nextPairStep returns the first emitted step whose participant is not yet the
// target build. Steps are followed in the checker's order, never reordered.
func nextPairStep(verdict releasecompat.Verdict, pool pairPool, target *pairRelease) (releasecompat.Step, bool, error) {
	current := map[string]pairParticipant{}
	for _, p := range pool.Participants {
		current[p.Name] = p
	}
	if len(verdict.Steps) != len(pool.Participants) {
		return releasecompat.Step{}, false, fmt.Errorf("preflight emitted %d steps for %d participants", len(verdict.Steps), len(pool.Participants))
	}
	// Validate the whole plan before choosing a step: each participant exactly once.
	seen := map[string]bool{}
	for _, step := range verdict.Steps {
		p, ok := current[step.Participant]
		if !ok || p.Role != step.Role {
			return step, false, fmt.Errorf("preflight step names unknown participant %s/%s", step.Role, step.Participant)
		}
		if seen[step.Participant] {
			return step, false, fmt.Errorf("preflight plan names %s more than once", step.Participant)
		}
		seen[step.Participant] = true
	}
	for _, step := range verdict.Steps {
		p := current[step.Participant]
		if step.Version != target.Version {
			return step, false, fmt.Errorf("preflight step %s targets %s, not %s", step.Participant, step.Version, target.Version)
		}
		if p.Version != target.Version || p.Commit != target.Commit {
			return step, true, nil
		}
	}
	return releasecompat.Step{}, false, nil
}

// pairRollout repeats inventory and the target preflight before every
// mutation, including standby replenishment, and once more at the end. A
// refusal returns before the step it would have authorized.
func pairRollout(ctx context.Context, a pairActions, target *pairRelease, operation string, record func(pairCheck)) error {
	const limit = 32
	for check := 1; ; check++ {
		if check > limit {
			return fmt.Errorf("%s did not converge on %s after %d preflight checks", operation, target, limit)
		}
		pool, err := a.Inventory(ctx)
		if err != nil {
			return fmt.Errorf("%s inventory %d: %w", operation, check, err)
		}
		started := time.Now()
		verdict, err := a.Preflight(ctx, target, pool, operation)
		checked := time.Since(started)
		if err != nil {
			return fmt.Errorf("%s preflight %d: %w", operation, check, err)
		}
		if !verdict.Allowed {
			refusal := releasecompat.Refusal{Code: "invalid_input", Reason: "refusal without a typed code"}
			if verdict.Refusal != nil {
				refusal = *verdict.Refusal
			}
			return &pairRefusal{Operation: operation, Check: check, Refusal: refusal}
		}
		entry := pairCheck{Operation: operation, Check: check, Preflight: checked}
		if a.StandbyMissing() {
			entry.Role, entry.Action = "relay", "replenish-standby"
			started = time.Now()
			err = a.ReplenishStandby(ctx, target)
			entry.Duration = time.Since(started)
			record(entry)
			if err != nil {
				return fmt.Errorf("%s standby replenishment: %w", operation, err)
			}
			continue
		}
		step, pending, err := nextPairStep(verdict, pool, target)
		if err != nil {
			return fmt.Errorf("%s preflight %d: %w", operation, check, err)
		}
		if !pending {
			// Completion is the observed inventory, not only the checker's plan.
			for _, p := range pool.Participants {
				if p.Version != target.Version || p.Commit != target.Commit {
					return fmt.Errorf("%s ended with %s on %s %s, not %s", operation, p.Name, p.Version, p.Commit, target)
				}
			}
			entry.Action = "final-check"
			record(entry)
			return nil
		}
		entry.Participant, entry.Role, entry.Action = step.Participant, step.Role, "replace"
		started = time.Now()
		err = a.Apply(ctx, step, target)
		entry.Duration = time.Since(started)
		record(entry)
		if err != nil {
			return fmt.Errorf("%s %s %s: %w", operation, step.Role, step.Participant, err)
		}
	}
}

// runReleasePairCommand owns its run directory, Redis and every process. It
// accepts only the pair flags plus -bin and -redis-server.
func runReleasePairCommand(o releasePairOptions, bin, redisAddr, redisBinary string, flags int) error {
	if o.A == "" || o.B == "" {
		return errors.New("release-pair requires -pair-a and -pair-b")
	}
	if redisAddr != "" {
		return errors.New("release-pair owns a throwaway Redis; unset RELAIS_REDIS_ADDR and omit -redis")
	}
	allowed := 0
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "release-pair" || f.Name == "bin" || f.Name == "redis-server" || strings.HasPrefix(f.Name, "pair-") {
			allowed++
		}
	})
	if allowed != flags {
		return errors.New("release-pair combines only with -pair-*, -bin and -redis-server")
	}
	o.RedisBinary = redisBinary
	root, err := filepath.Abs(bin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "release-pair-")
	if err != nil {
		return err
	}
	fmt.Printf("process logs: %s\n", dir)
	manager := &clusterprocess.Manager{}
	ctx, cancel := manager.Context()
	defer cancel()
	if err := runReleasePair(ctx, manager, dir, o); err != nil {
		return fmt.Errorf("%w (logs: %s)", err, dir)
	}
	return nil
}

type releasePairOptions struct {
	A, B, ASHA256, BSHA256 string
	ExpectRefusal          string
	RedisBinary            string
	Calls                  int
	Warmup, Settle         time.Duration
}
