package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/stretchr/testify/require"
)

// fakePairPool models the inventory, the checker's role-ordered plan and the
// effect of each replacement. Its log shows what preceded every mutation.
type fakePairPool struct {
	participants []pairParticipant
	log          []string
	refuseAt     int
	checks       int
	standbyGone  bool
	stuck        bool
	editPlan     func(*releasecompat.Verdict)
}

func (f *fakePairPool) Inventory(context.Context) (pairPool, error) {
	f.log = append(f.log, "inventory")
	return pairPool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy", Participants: append([]pairParticipant(nil), f.participants...)}, nil
}

func (f *fakePairPool) Preflight(_ context.Context, target *pairRelease, pool pairPool, operation string) (releasecompat.Verdict, error) {
	f.checks++
	f.log = append(f.log, "preflight")
	if f.checks == f.refuseAt {
		return releasecompat.Verdict{Refusal: &releasecompat.Refusal{Code: "unsupported_pair", Reason: "not nominated"}}, nil
	}
	rank := map[string]int{"worker": 0, "relay": 1, "control": 2, "signal": 3}
	steps := pool.Participants
	sort.SliceStable(steps, func(i, j int) bool {
		if operation == "rollback" {
			return rank[steps[i].Role] > rank[steps[j].Role]
		}
		return rank[steps[i].Role] < rank[steps[j].Role]
	})
	verdict := releasecompat.Verdict{Allowed: true}
	for _, p := range steps {
		verdict.Steps = append(verdict.Steps, releasecompat.Step{Participant: p.Name, Role: p.Role, Version: target.Version})
	}
	if f.editPlan != nil {
		f.editPlan(&verdict)
	}
	return verdict, nil
}

func (f *fakePairPool) StandbyMissing() bool { return f.standbyGone }

func (f *fakePairPool) ReplenishStandby(_ context.Context, target *pairRelease) error {
	f.log = append(f.log, "replenish")
	f.standbyGone = false
	f.participants = append(f.participants, pairParticipant{Name: "relay-new", Role: "relay", Version: target.Version, Commit: target.Commit})
	return nil
}

func (f *fakePairPool) Apply(_ context.Context, step releasecompat.Step, target *pairRelease) error {
	f.log = append(f.log, "apply "+step.Participant)
	if f.stuck {
		return nil
	}
	for i, p := range f.participants {
		if p.Name != step.Participant {
			continue
		}
		if p.Name == "relay-active" {
			// The successor already runs the target; the old process is gone.
			f.participants = append(f.participants[:i], f.participants[i+1:]...)
			f.standbyGone = true
			return nil
		}
		f.participants[i].Version, f.participants[i].Commit = target.Version, target.Commit
	}
	return nil
}

func pairFakeTopology(version string) *fakePairPool {
	f := &fakePairPool{}
	for _, p := range [][2]string{{"worker-0", "worker"}, {"worker-1", "worker"}, {"relay-standby", "relay"}, {"relay-active", "relay"}, {"control", "control"}, {"signal", "signal"}} {
		f.participants = append(f.participants, pairParticipant{Name: p[0], Role: p[1], Version: version, Commit: "old"})
	}
	return f
}

func appliedPairSteps(log []string) []string {
	var out []string
	for _, entry := range log {
		if strings.HasPrefix(entry, "apply ") || entry == "replenish" {
			out = append(out, entry)
		}
	}
	return out
}

func TestPairRolloutFollowsEmittedOrderWithPreflightBeforeEveryMutation(t *testing.T) {
	f := pairFakeTopology("0.3.0")
	var checks []pairCheck
	err := pairRollout(context.Background(), f, &pairRelease{Label: "B", Version: "0.3.1", Commit: "new"}, "upgrade", func(c pairCheck) { checks = append(checks, c) })
	require.NoError(t, err)
	require.Equal(t, []string{"apply worker-0", "apply worker-1", "apply relay-standby", "apply relay-active", "replenish", "apply control", "apply signal"}, appliedPairSteps(f.log))
	for i, entry := range f.log {
		if strings.HasPrefix(entry, "apply ") || entry == "replenish" {
			require.Equal(t, "preflight", f.log[i-1], "every mutation follows a fresh preflight")
			require.Equal(t, "inventory", f.log[i-2], "every preflight reads a fresh inventory")
		}
	}
	require.Equal(t, "preflight", f.log[len(f.log)-1], "the final state is checked too")
	require.Equal(t, "final-check", checks[len(checks)-1].Action)

	f.log = nil
	err = pairRollout(context.Background(), f, &pairRelease{Label: "A", Version: "0.3.0", Commit: "old"}, "rollback", func(pairCheck) {})
	require.NoError(t, err)
	require.Equal(t, []string{"apply signal", "apply control", "apply relay-standby", "apply relay-new", "apply worker-0", "apply worker-1"}, appliedPairSteps(f.log), "rollback follows the reversed emitted role order")
}

func TestPairRolloutRefusalStopsBeforeMutation(t *testing.T) {
	for _, refuseAt := range []int{1, 3} {
		f := pairFakeTopology("0.3.0")
		f.refuseAt = refuseAt
		err := pairRollout(context.Background(), f, &pairRelease{Version: "0.3.1", Commit: "new"}, "upgrade", func(pairCheck) {})
		var refusal *pairRefusal
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, "unsupported_pair", refusal.Refusal.Code)
		require.Equal(t, refuseAt, refusal.Check)
		require.Len(t, appliedPairSteps(f.log), refuseAt-1, "the refused check authorizes nothing")
		require.Equal(t, "preflight", f.log[len(f.log)-1])
	}
}

// A plan of the right length that names control twice and omits signal must
// stop before its first step, even though that step looks valid.
func TestPairRolloutRefusesAPlanThatSkipsAParticipant(t *testing.T) {
	f := pairFakeTopology("0.3.0")
	f.editPlan = func(v *releasecompat.Verdict) {
		for i, step := range v.Steps {
			if step.Role == "signal" {
				v.Steps[i] = releasecompat.Step{Participant: "control", Role: "control", Version: step.Version}
			}
		}
	}
	err := pairRollout(context.Background(), f, &pairRelease{Version: "0.3.1", Commit: "new"}, "upgrade", func(pairCheck) {})
	require.ErrorContains(t, err, "names control more than once")
	require.Empty(t, appliedPairSteps(f.log), "no participant changes on an incomplete plan")
}

func TestPairRolloutStopsWhenReplacementDoesNotConverge(t *testing.T) {
	f := pairFakeTopology("0.3.0")
	f.stuck = true
	err := pairRollout(context.Background(), f, &pairRelease{Version: "0.3.1", Commit: "new"}, "upgrade", func(pairCheck) {})
	require.ErrorContains(t, err, "did not converge")
}

func TestNextPairStepRefusesAPlanItCannotFollow(t *testing.T) {
	target := &pairRelease{Version: "0.3.1", Commit: "new"}
	pool := pairPool{Participants: []pairParticipant{{Name: "worker-0", Role: "worker", Version: "0.3.0", Commit: "old"}}}
	_, _, err := nextPairStep(releasecompat.Verdict{Allowed: true, Steps: []releasecompat.Step{{Participant: "ghost", Role: "worker", Version: "0.3.1"}}}, pool, target)
	require.ErrorContains(t, err, "unknown participant")
	_, _, err = nextPairStep(releasecompat.Verdict{Allowed: true, Steps: []releasecompat.Step{{Participant: "worker-0", Role: "worker", Version: "0.3.2"}}}, pool, target)
	require.ErrorContains(t, err, "targets 0.3.2")
	_, _, err = nextPairStep(releasecompat.Verdict{Allowed: true}, pool, target)
	require.ErrorContains(t, err, "emitted 0 steps")
	step, pending, err := nextPairStep(releasecompat.Verdict{Allowed: true, Steps: []releasecompat.Step{{Participant: "worker-0", Role: "worker", Version: "0.3.1"}}}, pool, target)
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, "worker-0", step.Participant)
	pool.Participants[0].Commit = "other"
	pool.Participants[0].Version = "0.3.1"
	_, pending, err = nextPairStep(releasecompat.Verdict{Allowed: true, Steps: []releasecompat.Step{{Participant: "worker-0", Role: "worker", Version: "0.3.1"}}}, pool, target)
	require.NoError(t, err)
	require.True(t, pending, "a same-version build with another commit is not the target")
}

func writePairCompat(t *testing.T, stdout string, exit int) string {
	t.Helper()
	dir := t.TempDir()
	reply := filepath.Join(dir, "reply.json")
	require.NoError(t, os.WriteFile(reply, []byte(stdout), 0600))
	script := filepath.Join(dir, "relais-compat")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ncat '"+reply+"'\nexit "+string(rune('0'+exit))+"\n"), 0700))
	return script
}

func TestRunPairPreflightSeparatesTypedRefusalsFromBrokenReplies(t *testing.T) {
	ctx := context.Background()
	verdict, _, err := runPairPreflight(ctx, writePairCompat(t, `{"refusal":{"code":"unsupported_pair","reason":"x"}}`, 1), "m", "p", "upgrade")
	require.NoError(t, err)
	require.False(t, verdict.Allowed)
	require.Equal(t, "unsupported_pair", verdict.Refusal.Code)
	verdict, _, err = runPairPreflight(ctx, writePairCompat(t, `{"allowed":true,"steps":[]}`, 0), "m", "p", "upgrade")
	require.NoError(t, err)
	require.True(t, verdict.Allowed)
	_, _, err = runPairPreflight(ctx, writePairCompat(t, `{"allowed":true}`, 1), "m", "p", "upgrade")
	require.ErrorContains(t, err, "disagrees")
	_, _, err = runPairPreflight(ctx, writePairCompat(t, `{"allowed":false}`, 0), "m", "p", "upgrade")
	require.ErrorContains(t, err, "disagrees", "a refusal must carry a typed code")
	_, _, err = runPairPreflight(ctx, writePairCompat(t, `not json`, 1), "m", "p", "upgrade")
	require.ErrorContains(t, err, "unreadable")
}

func TestParseBinaryIdentityRequiresVersionAndCommit(t *testing.T) {
	version, commit, err := parseBinaryIdentity("relais-worker version=0.3.1 commit=abc date=2026\n")
	require.NoError(t, err)
	require.Equal(t, "0.3.1", version)
	require.Equal(t, "abc", commit)
	_, _, err = parseBinaryIdentity("relais-worker version=0.3.1")
	require.Error(t, err)
}

// The inventory must satisfy the strict offline decoder, and the real checker
// must refuse an unnominated pair and allow a nominated one.
func TestPairInventoryMatchesTheCheckerContract(t *testing.T) {
	template, err := os.ReadFile("../../scripts/release/compatibility.json")
	require.NoError(t, err)
	stamp := func(version, commit string, peers ...string) *pairRelease {
		var m map[string]any
		require.NoError(t, json.Unmarshal(template, &m))
		m["version"], m["commit"], m["os"], m["arch"] = version, commit, "linux", "amd64"
		m["oldest_live_peer"], m["oldest_rollback"] = peers[len(peers)-1], peers[len(peers)-1]
		m["tested_peers"] = peers
		raw, err := json.Marshal(m)
		require.NoError(t, err)
		return &pairRelease{Version: version, Commit: commit, SHA256: strings.Repeat(commit[:1], 64), Manifest: raw}
	}
	older := stamp("0.3.0", strings.Repeat("a", 40), "0.3.0")
	plain := stamp("0.3.1", strings.Repeat("b", 40), "0.3.1")
	nominating := stamp("0.3.1", strings.Repeat("c", 40), "0.3.1", "0.3.0")
	pool := pairPool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy"}
	for _, p := range [][2]string{{"worker-0", "worker"}, {"worker-1", "worker"}, {"relay-2", "relay"}, {"relay-1", "relay"}, {"control", "control"}, {"signal", "signal"}} {
		pool.Participants = append(pool.Participants, newPairParticipant(p[0], p[1], older.Version, older.Commit, older))
	}
	require.Equal(t, "legacy", pool.Participants[0].Profile)
	require.Equal(t, []string{"echo"}, pool.Participants[0].Agents)
	require.True(t, pool.Participants[0].DefaultBudgets)
	require.Equal(t, "default", pool.Participants[2].Profile)
	require.Empty(t, pool.Participants[2].Agents)
	encoded, err := json.Marshal(pool)
	require.NoError(t, err)
	var decoded releasecompat.Pool
	require.NoError(t, releasecompat.Decode(encoded, &decoded))
	var target releasecompat.Manifest
	require.NoError(t, releasecompat.Decode(plain.Manifest, &target))
	verdict := releasecompat.Preflight(decoded, target, "upgrade")
	require.False(t, verdict.Allowed)
	require.Equal(t, "unsupported_pair", verdict.Refusal.Code)
	require.NoError(t, releasecompat.Decode(nominating.Manifest, &target))
	verdict = releasecompat.Preflight(decoded, target, "upgrade")
	require.True(t, verdict.Allowed, "%+v", verdict.Refusal)
	require.Equal(t, "relay-2", verdict.Steps[2].Participant, "standby is listed, and therefore replaced, before the active relay")
	require.Equal(t, "relay-1", verdict.Steps[3].Participant)
}

func TestRunReleasePairCommandRefusesForeignModesAndSharedRedis(t *testing.T) {
	err := runReleasePairCommand(releasePairOptions{A: "a", B: "b"}, t.TempDir(), "127.0.0.1:16379", "redis-server", 0)
	require.ErrorContains(t, err, "owns a throwaway Redis")
	err = runReleasePairCommand(releasePairOptions{A: "a"}, t.TempDir(), "", "redis-server", 0)
	require.ErrorContains(t, err, "requires -pair-a and -pair-b")
	err = runReleasePairCommand(releasePairOptions{A: "a", B: "b"}, t.TempDir(), "", "redis-server", 1)
	require.ErrorContains(t, err, "combines only with")
}
