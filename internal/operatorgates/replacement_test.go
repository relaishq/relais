package operatorgates

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

// trialCluster is a small control plane for whole trial loops. Every step costs
// one second; a kill of the call owner records one fresh takeover receipt.
type trialCluster struct {
	g         *procedure
	clock     *FakeClock
	call      controlplane.CallStatus
	dead      map[string]bool
	draining  map[string]bool
	takeovers []controlplane.MoveResult
	history   []json.RawMessage
	ops       []string
	kills     int
	hungUp    bool
	wide      map[int]bool // 1-based kill numbers with an excluded bracket
}

func newTrialCluster(t *testing.T) *trialCluster {
	t.Helper()
	g, clock := testProcedure(t)
	c := &trialCluster{g: g, clock: clock, call: controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1}, dead: map[string]bool{}, draining: map[string]bool{}}
	g.p.Step = c.step
	g.p.Media = func(context.Context) (*MediaSummary, error) {
		c.hungUp = true // The caller's media ends before the final status read.
		return &MediaSummary{Status: operator.Passed}, nil
	}
	return c
}

func (c *trialCluster) step(_ context.Context, op, _ string, _ time.Duration, extra ProcessExtra) (StepResult, error) {
	c.ops = append(c.ops, op+":"+extra.Worker)
	c.clock.Advance(time.Second)
	r := StepResult{Started: c.clock.Wall(), Ended: c.clock.Wall()}
	switch op {
	case "status":
		r.LossKnown = true
		r.Status = controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{c.call}, Takeovers: slices.Clone(c.takeovers)}
		r.History = slices.Clone(c.history)
		if c.hungUp {
			r.Status.Calls = nil
		}
		for _, w := range []string{"w1", "w2"} {
			r.Status.Workers = append(r.Status.Workers, controlplane.WorkerStatus{Name: w, Address: c.g.c.Parameters.Workers[w].Media, Dead: c.dead[w], Draining: c.draining[w]})
		}
	case "move":
		from := c.call.Owner
		c.call.Owner, c.call.Epoch = extra.Worker, c.call.Epoch+1
		r.Move = controlplane.MoveResult{ID: c.call.ID, From: from, To: extra.Worker, Kind: "move", Start: c.clock.Wall(), End: c.clock.Wall()}
	case "drain":
		c.draining[extra.Worker] = true
		if c.call.Owner == extra.Worker {
			m := controlplane.MoveResult{ID: c.call.ID, From: extra.Worker, To: other(extra.Worker), Kind: "move", Start: c.clock.Wall(), End: c.clock.Wall()}
			r.Drain.Moves = []controlplane.MoveResult{m}
			r.Moves = []json.RawMessage{oracleJSON(m)}
			c.call.Owner, c.call.Epoch = m.To, c.call.Epoch+1
		}
	case "worker-down":
		c.kills++
		c.dead[extra.Worker] = true
		r.KillExcluded = c.wide[c.kills]
		if c.call.Owner == extra.Worker {
			m := controlplane.MoveResult{ID: c.call.ID, From: extra.Worker, To: other(extra.Worker), Kind: "takeover", Start: c.clock.Wall(), End: c.clock.Wall()}
			c.takeovers = append(c.takeovers, m)
			c.history = append(c.history, oracleJSON(m))
			c.call.Owner, c.call.Epoch = m.To, c.call.Epoch+1
		}
	case "worker-stop":
		c.dead[extra.Worker] = true
	case "worker-start":
		c.dead[extra.Worker], c.draining[extra.Worker] = false, false
	case "control-start":
		ready := int64(1)
		r.ReadyMS = &ready
	case "control-stop", "replace", "worker-empty":
	}
	return r, nil
}

func (c *trialCluster) count(op string) int {
	n := 0
	for _, o := range c.ops {
		if len(o) > len(op) && o[:len(op)+1] == op+":" {
			n++
		}
	}
	return n
}

func receiptKinds(b GateBroker) (moves, takeovers int) {
	for _, r := range b.Events() {
		switch r.Kind {
		case "move":
			moves++
		case "takeover":
			takeovers++
		}
	}
	return moves, takeovers
}

// Characterization: without an excluded kill, trials run exactly their targets.
func TestTrialsRunExactTargets(t *testing.T) {
	c := newTrialCluster(t)
	if err := c.g.trials(context.Background(), TrialTarget{Moves: 7, Takeovers: 3}, "rollout"); err != nil {
		t.Fatal(err)
	}
	moves, takeovers := receiptKinds(c.g.p.Broker)
	if moves != 7 || takeovers != 3 || c.count("worker-down") != 3 || c.count("move") != 7 {
		t.Fatalf("trial targets changed: moves=%d takeovers=%d ops=%v", moves, takeovers, c.ops)
	}
}

// Characterization: the control restart's own takeover counts toward its target.
func TestControlRestartTrialsCountGateOwnedTakeover(t *testing.T) {
	c := newTrialCluster(t)
	c.g.c.Trials = true
	r := RunProcessGate(context.Background(), operator.ControlRestart, c.g.c, c.g.p, c.g.end)
	moves, takeovers := receiptKinds(c.g.p.Broker)
	if r.Status != operator.Passed || moves != 7 || takeovers != 4 {
		t.Fatalf("control restart trial population changed: %+v moves=%d takeovers=%d", r, moves, takeovers)
	}
}

func assertReplacements(t *testing.T, c *trialCluster, marker string, kills, takeovers, excluded, replaced int, skipped string) {
	t.Helper()
	_, got := receiptKinds(c.g.p.Broker)
	e := c.g.evidence
	if c.count("worker-down") != kills || got != takeovers || e.ExcludedKills != excluded || e.KillReplacements != replaced || e.KillReplacementSkipped != skipped {
		t.Fatalf("MUTATION: %s: kills=%d takeovers=%d evidence=%+v", marker, c.count("worker-down"), got, e)
	}
}

// An excluded takeover adds exactly one more takeover iteration.
func TestExcludedKillSchedulesReplacement(t *testing.T) {
	c := newTrialCluster(t)
	c.wide = map[int]bool{2: true}
	if err := c.g.trials(context.Background(), TrialTarget{Moves: 7, Takeovers: 3}, "rollout"); err != nil {
		t.Fatal(err)
	}
	assertReplacements(t, c, "replacement-not-scheduled", 4, 4, 1, 1, "")
	if c.g.c.KillReplacements != 1 {
		t.Fatal("replacement not charged to the run budget")
	}
}

// A wide replacement owes its own replacement, but never more than two per run.
func TestReplacementBudgetIsRunWide(t *testing.T) {
	c := newTrialCluster(t)
	c.wide = map[int]bool{1: true, 3: true, 4: true, 5: true}
	if err := c.g.trials(context.Background(), TrialTarget{Moves: 1, Takeovers: 3}, "rollout"); err != nil {
		t.Fatal(err)
	}
	assertReplacements(t, c, "replacement-budget", 5, 5, 4, 2, "replacement-budget")

	// A later gate sharing the run configuration gets no third replacement.
	later := newTrialCluster(t)
	later.g.c.KillReplacements = MaxKillReplacements
	later.wide = map[int]bool{1: true}
	if err := later.g.trials(context.Background(), TrialTarget{Moves: 0, Takeovers: 2}, "control-restart"); err != nil {
		t.Fatal(err)
	}
	assertReplacements(t, later, "replacement-budget", 2, 2, 1, 0, "replacement-budget")
}

// A replacement starts only when the measured cycle plus one more post-event
// media window fits before the procedure end and the caller's media end.
// Skipping it leaves the gate to finish without a deadline.
func TestReplacementSkippedWhenTimeShort(t *testing.T) {
	probe := newTrialCluster(t)
	if err := probe.g.trials(context.Background(), TrialTarget{Moves: 7, Takeovers: 3}, "rollout"); err != nil {
		t.Fatal(err)
	}
	finished, cycle := probe.clock.Elapsed(), probe.g.takeoverCycle
	if cycle < ConsentWindow {
		t.Fatal("measured cycle omits its consent window", cycle)
	}
	for _, limit := range []string{"procedure", "media"} {
		for _, room := range []time.Duration{-time.Second, 0} {
			t.Run(limit+"/"+room.String(), func(t *testing.T) {
				c := newTrialCluster(t)
				c.wide = map[int]bool{3: true}
				if limit == "procedure" {
					c.g.end = finished + cycle + ConsentWindow + room
				} else {
					start := c.clock.Now.Add(finished + cycle + ConsentWindow + room - CampaignDuration(c.g.gate)).UnixMilli()
					c.g.p.Broker.(*testGateBroker).reg.MediaStartedAt = &start
				}
				err := c.g.trials(context.Background(), TrialTarget{Moves: 7, Takeovers: 3}, "rollout")
				if err != nil {
					t.Fatal("MUTATION: replacement-time-guard", err)
				}
				if room < 0 {
					assertReplacements(t, c, "replacement-time-guard", 3, 3, 1, 0, "replacement-time")
				} else {
					assertReplacements(t, c, "replacement-time-guard", 4, 4, 1, 1, "")
				}
			})
		}
	}
}

// Control restart's own excluded takeover is made up by the trials after it.
// Its earlier unarmed degraded-restart kill never owes a replacement.
func TestExcludedGateOwnedKillMadeUpByTrials(t *testing.T) {
	for _, tc := range []struct {
		wide                int
		takeovers, replaced int
	}{{1, 4, 0}, {2, 5, 1}} {
		t.Run(fmt.Sprint(tc.wide), func(t *testing.T) {
			c := newTrialCluster(t)
			c.g.c.Trials = true
			c.wide = map[int]bool{tc.wide: true}
			// Media starts two minutes after registration, leaving room for one more cycle.
			media := c.clock.Now.Add(2 * time.Minute).UnixMilli()
			c.g.p.Broker.(*testGateBroker).reg.MediaStartedAt = &media
			r := RunProcessGate(context.Background(), operator.ControlRestart, c.g.c, c.g.p, c.g.end)
			moves, takeovers := receiptKinds(c.g.p.Broker)
			if r.Status != operator.Passed || r.Reason != "" || moves != 7 || takeovers != tc.takeovers || r.Process.KillReplacements != tc.replaced {
				t.Fatalf("MUTATION: gate-owned-kill-not-replaced: %+v moves=%d takeovers=%d", r, moves, takeovers)
			}
		})
	}
}
