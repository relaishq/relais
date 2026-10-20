package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/workerdriver"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
)

type studyAges struct {
	CheckpointP99 float64 `json:"checkpoint_age_p99_ms"`
	SnapshotP99   float64 `json:"snapshot_age_p99_ms"`
	CheckpointMax float64 `json:"checkpoint_age_max_ms"`
	SnapshotMax   float64 `json:"snapshot_age_max_ms"`
	Coverage      bool    `json:"coverage_complete"`
	Samples       int     `json:"samples"`
}
type splitObservation struct {
	Ages            studyAges            `json:"ages"`
	Inputs          map[string][2]uint64 `json:"accepted_input_progress"`
	LiveCalls       int                  `json:"live_calls"`
	LostCalls       uint64               `json:"lost_calls"`
	Arm             string               `json:"arm"`
	Scenario        string               `json:"scenario"`
	WorkerArgs      []string             `json:"worker_args"`
	SetupFinishedAt time.Time            `json:"setup_finished_at"`
	Store           storeTrace           `json:"store"`
	Inventory       map[byte]uint64      `json:"live_key_id_inventory,omitempty"`
	InventoryCalls  int                  `json:"inventory_live_calls,omitempty"`
	InventoryAt     time.Time            `json:"inventory_at,omitzero"`
	Survivors       map[string]int       `json:"survivors,omitempty"`
	Refused         uint64               `json:"history_refusals"`
	InputSamples    int                  `json:"fixture_samples"`
}
type splitCapacityStep struct {
	capacityStep
	Measurement *splitObservation `json:"measurement"`
}
type splitCapacityReport struct {
	Kind             string              `json:"kind"`
	Runner           string              `json:"runner"`
	GOOS             string              `json:"goos"`
	Topology         string              `json:"topology"`
	Note             string              `json:"note"`
	Outcome          string              `json:"outcome"`
	Build            map[string]string   `json:"binary_sha256"`
	Calls            int                 `json:"callers"`
	DurationSeconds  float64             `json:"duration_seconds"`
	StoreOpsBudget   float64             `json:"store_commands_budget_per_call_second"`
	StoreBytesBudget float64             `json:"store_bytes_budget_per_call_second"`
	Steps            []splitCapacityStep `json:"steps"`
}

func studyWorker(bin, arm, scenario string) workerdriver.Options {
	o := workerdriver.Options{Binary: filepath.Join(bin, "delayed-loopback"), Subcommand: "history", Agent: "history-loopback/v1"}
	if scenario == "history-churn" {
		o.Agent = "history-churn/v1"
	}
	if scenario == "echo" {
		o = workerdriver.Options{Agent: "echo"}
	}
	if arm == "after" {
		o.Args = []string{"-agent-split-checkpoints"}
	} else if scenario != "echo" {
		o.Args = []string{"-agent-max-snapshot-bytes=24576", "-agent-snapshot-bytes-per-second=270336", "-agent-snapshot-burst-bytes=98304"}
	}
	return o
}
func runSplitCapacity(ctx context.Context, m *clusterprocess.Manager, c config, root string) error {
	host, _ := os.Hostname()
	result := splitCapacityReport{Kind: "split-capacity", Runner: host, GOOS: runtime.GOOS, Topology: "loopback-processes", Note: "One runner's matched fixed-load measurements. Same binaries, bitrate, buffers, call count and topology; separate fresh owned Redis for each step. No capacity ramp or managed-store claim. Echo is compared separately with stock defaults and the split admission switch off/on. Mac results are smoke only; Linux thread CPU and hosted run remain separate gates.", Outcome: "incomplete", Calls: c.Callers, DurationSeconds: c.Duration.Seconds(), StoreOpsBudget: 100, StoreBytesBudget: 128 << 10, Build: map[string]string{}}
	for _, name := range []string{"relais-load", "delayed-loopback", "relais-worker", "relais-control", "relais-relay"} {
		data, err := os.ReadFile(filepath.Join(c.Bin, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		result.Build[name] = hex.EncodeToString(sum[:])
	}
	// Each matched pair is adjacent and has identical duration and placement.
	cases := [][2]string{{"before", "steady"}, {"after", "steady"}, {"before", "echo"}, {"after", "echo"}, {"after", "history-churn"}, {"after", "rotation"}, {"after", "n-plus-one"}, {"after", "drain"}}
	var allErr error
	for i, item := range cases {
		if err := ctx.Err(); err != nil {
			return errors.Join(allErr, err)
		}
		arm, scenario := item[0], item[1]
		dir := filepath.Join(root, fmt.Sprintf("%02d-%s-%s", i, arm, scenario))
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		stepConfig := c
		stepConfig.Capacity = true
		stepConfig.SplitCapacity = false
		stepConfig.Worker = studyWorker(c.Bin, arm, scenario)
		stepConfig.Study = &splitObservation{Arm: arm, Scenario: scenario, WorkerArgs: stepConfig.Worker.Args}
		stepConfig.Events = []loadgen.Event{{After: 6 * time.Second, Kind: "move", Count: 1, Worker: 2}}
		if scenario == "rotation" {
			stepConfig.Duration = max(c.Duration, 65*time.Second)
			stepConfig.Events = nil
		}
		if scenario == "n-plus-one" {
			stepConfig.Events = []loadgen.Event{{After: 6 * time.Second, Kind: "kill", Worker: 0}, {After: 12 * time.Second, Kind: "move", Count: 1, Worker: -1}}
		}
		if scenario == "drain" {
			stepConfig.Events = []loadgen.Event{{After: 6 * time.Second, Kind: "drain", Worker: 0}}
		}
		if scenario == "n-plus-one" || scenario == "drain" {
			stepConfig.PlaceOnWorker = true
		}
		stepCtx, cancel := context.WithTimeout(ctx, stepConfig.Duration+3*time.Minute)
		raw, runErr := execute(stepCtx, m, stepConfig, dir)
		cancel()
		kind := "relay"
		if scenario == "n-plus-one" || scenario == "drain" {
			kind = scenario
		}
		judged := judgeSplitCapacity(kind, raw, customCapacity(stepConfig))
		if customCapacity(stepConfig) {
			judged.Custom = customEvidence(raw)
		}
		collectFixtureEvidence(raw, stepConfig.Study)
		if runErr != nil {
			judged.Causes = append(judged.Causes, runErr.Error())
			judged.Verdict = "inconclusive"
			allErr = errors.Join(allErr, runErr)
		}
		if !raw.Leaks.Checked {
			judged.Causes = append(judged.Causes, "cleanup did not pass")
			judged.Verdict = "fail"
		}
		result.Steps = append(result.Steps, splitCapacityStep{capacityStep: judged, Measurement: stepConfig.Study})
		if err := writeJSON(filepath.Join(dir, "split-step.json"), result.Steps[len(result.Steps)-1]); err != nil {
			return err
		}
		if err := writeJSON(c.Output, result); err != nil {
			return err
		}
		fmt.Printf("split-capacity arm=%s scenario=%s service=%s generator=%s commands/call-s=%.2f bytes/call-s=%.0f cleanup=%t\n", arm, scenario, judged.Verdict, judged.GeneratorOutcome, judged.StoreCommandsPerCallSecond, judged.StoreBytesPerCallSecond, raw.Leaks.Checked)
	}
	if allErr == nil {
		result.Outcome = "complete"
	}
	return errors.Join(allErr, writeJSON(c.Output, result))
}
func collectFixtureEvidence(r loadgen.RunReport, o *splitObservation) {
	var checkpoints, snapshots []float64
	coverage := map[string]int{}
	for _, sample := range r.Telemetry.Agents.Samples {
		coverage[sample.Status.ID]++
		checkpoints = append(checkpoints, sample.Status.CheckpointAgeMS)
		snapshots = append(snapshots, sample.Status.SnapshotAgeMS)
	}
	o.Ages.Samples = len(checkpoints)
	if len(checkpoints) > 0 {
		slices.Sort(checkpoints)
		slices.Sort(snapshots)
		idx := int(math.Ceil(float64(len(checkpoints))*.99)) - 1
		o.Ages.CheckpointP99 = checkpoints[idx]
		o.Ages.SnapshotP99 = snapshots[idx]
		o.Ages.CheckpointMax = checkpoints[len(checkpoints)-1]
		o.Ages.SnapshotMax = snapshots[len(snapshots)-1]
	}
	o.Ages.Coverage = len(coverage) == r.Callers && len(r.Telemetry.Agents.Errors) == 0 && r.Telemetry.Agents.Missing <= uint64(math.Ceil(float64(r.Callers)*r.Duration.Seconds()))
	for _, p := range r.Processes {
		for _, call := range p.Calls {
			if coverage[call.Session] < int(r.Duration.Seconds()*9) {
				o.Ages.Coverage = false
			}
		}
	}

	latest := map[string]uint64{}
	o.Inputs = map[string][2]uint64{}
	for _, sample := range r.Telemetry.Agents.Samples {
		var state struct {
			Refused uint64 `json:"refused"`
			Inputs  uint64 `json:"inputs"`
		}
		if json.Unmarshal(sample.Status.App, &state) == nil {
			o.InputSamples++
			progress, ok := o.Inputs[sample.Status.ID]
			if !ok {
				progress[0] = state.Inputs
			}
			progress[1] = state.Inputs
			o.Inputs[sample.Status.ID] = progress
			latest[sample.Status.ID] = max(latest[sample.Status.ID], state.Refused)
		}
	}
	for _, n := range latest {
		o.Refused += n
	}
}
func runStudyRotation(ctx context.Context, t *topology, generators []generator, c config, r *loadgen.RunReport) error {
	if err := waitUntil(ctx, r.StartedAt.Add(6*time.Second)); err != nil {
		return err
	}
	t.Observer.mark("rotation")
	for id := range c.Workers {
		var status controlplane.Status
		if err := request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status); err != nil {
			return err
		}
		occupied := false
		for _, call := range status.Calls {
			occupied = occupied || call.Owner == strconv.Itoa(id)
		}
		if occupied {
			e := loadgen.Event{After: time.Since(r.StartedAt), Kind: "drain", Worker: id}
			event := runEvent(ctx, t, generators, e)
			r.Events = append(r.Events, event)
			if event.Error != "" {
				return errors.New(event.Error)
			}
		} else {
			var drained controlplane.DrainResult
			if err := request(ctx, http.MethodPost, strings.TrimSuffix(t.External.SignalingURL, "/calls")+fmt.Sprintf("/workers/%d/drain", id), nil, &drained); err != nil {
				return err
			}
			if drained.Error != "" {
				return errors.New(drained.Error)
			}
		}
		launch := t.Launch[id]
		launch.Env = append(withoutEnv(withoutEnv(withoutEnv(launch.Env, "RELAIS_SESSIONSTORE_KEY"), "RELAIS_SESSIONSTORE_KEY_ID"), "RELAIS_SESSIONSTORE_OLD_KEYS"), "RELAIS_SESSIONSTORE_KEY="+hex.EncodeToString(t.NewKey[:]), "RELAIS_SESSIONSTORE_KEY_ID=2", `RELAIS_SESSIONSTORE_OLD_KEYS={"1":"`+hex.EncodeToString(t.OldKey[:])+`"}`)
		if t.Monitor != nil {
			t.Monitor.SampleNow(ctx)
			t.Monitor.Retire(fmt.Sprintf("worker-%d", id))
		}
		if err := t.recycle(ctx, id); err != nil {
			return err
		}
		if err := waitUntil(ctx, time.Now().Add(3*time.Second)); err != nil {
			return err
		}
	}
	if err := waitUntil(ctx, time.Now().Add(time.Second)); err != nil {
		return err
	}
	var status controlplane.Status
	if err := request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status); err != nil {
		return err
	}
	c.Study.Survivors = map[string]int{}
	for _, call := range status.Calls {
		c.Study.Survivors[call.Owner]++
	}
	store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: t.RedisAddr, Password: t.RedisPassword, Prefix: t.Prefix}, t.NewKey[:], sessionstore.RedisOptions{KeyID: 2, OldKeys: map[byte][]byte{1: t.OldKey[:]}})
	if err != nil {
		return err
	}
	inventory, err := store.ComponentKeyInventory(ctx)
	err = errors.Join(err, store.Close())
	if err != nil {
		return err
	}
	c.Study.Inventory = inventory
	c.Study.InventoryCalls = len(status.Calls)
	c.Study.InventoryAt = time.Now()
	t.Observer.mark("steady")
	if len(status.Calls) != c.Callers || status.LostCount != 0 || inventory[1] != 0 || inventory[2] != uint64(5*c.Callers) {
		return errors.New("rotation lost calls or live references remain off the new key")
	}
	return nil
}

func observeStudySurvivors(ctx context.Context, t *topology, c config) error {
	var status controlplane.Status
	if err := request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status); err != nil {
		return err
	}
	c.Study.Survivors = map[string]int{}
	for _, call := range status.Calls {
		c.Study.Survivors[call.Owner]++
	}
	c.Study.LiveCalls = len(status.Calls)
	c.Study.LostCalls = uint64(status.LostCount)
	return nil
}

func judgeSplitCapacity(kind string, raw loadgen.RunReport, custom bool) capacityStep {
	generator, tolerated := capacityGenerator(raw)
	view := generator
	if custom {
		view = asyncCapacityView(generator)
	}
	judged := judgeCapacityStep(kind, view, 100, 128<<10, nil, true)
	judged.Run = raw
	judged.ToleratedSlips = tolerated
	return judged
}
