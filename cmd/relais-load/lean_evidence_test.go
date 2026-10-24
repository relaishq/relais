package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestLeanGapInventoryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, want, cause string
		recorded          bool
		overflow          int
	}{
		{"unrecorded", "inconclusive", "gap inventory unavailable", false, 0},
		{"overflow", "fail", "gap inventory overflow", true, 7},
		{"unrecorded overflow", "fail", "gap inventory overflow", false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := leanFixture(t)
			track := &m.Phases[0].Run.Processes[0].Calls[0].Call.Tracks[0]
			track.MediaGapsRecorded, track.MediaGapOverflow = tc.recorded, tc.overflow
			r := judgeLean(m)
			require.Equal(t, tc.want, r.Outcome, leanCauses(r))
			require.Contains(t, leanCauses(r), tc.cause)
		})
	}
}

func TestLeanCloudAllowanceDefault(t *testing.T) {
	m := leanFixture(t)
	// Omitting the topology and the old opt-in cannot waive cloud evidence.
	require.NoError(t, json.Unmarshal([]byte(`{"topology":""}`), &m))
	m.ENARequired = false
	r := judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
	require.Contains(t, leanCauses(r), "ENA")
	require.Equal(t, "inconclusive", r.CloudQualification.Outcome)
	for i := range m.Phases {
		p := &m.Phases[i]
		for _, worker := range append([]leanWorker{{Name: "relay", Generation: "relay-generation"}, {Name: "control", Generation: "control-generation"}}, p.Before.Workers...) {
			row := leanAllowance{Host: worker.Name, Generation: worker.Generation, First: map[string]float64{}, Last: map[string]float64{}, ConntrackRemaining: leanPtr(100.0)}
			for _, key := range leanENACounters {
				row.First[key], row.Last[key] = 0, 0
			}
			p.Allowances = append(p.Allowances, row)
		}
	}
	r = judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	require.Equal(t, "pass", r.CloudQualification.Outcome)
	m.Phases[0].Allowances[0].Last[leanENACounters[0]] = 1
	r = judgeLean(m)
	require.Equal(t, "fail", r.Outcome, leanCauses(r))
	require.Equal(t, "fail", r.CloudQualification.Outcome)
}

func TestLeanLocalCloudQualification(t *testing.T) {
	m := leanFixture(t)
	require.NoError(t, json.Unmarshal([]byte(`{"topology":"local"}`), &m))
	r := judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	b, err := json.Marshal(r)
	require.NoError(t, err)
	var report map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &report))
	var qualification leanVerdict
	require.NoError(t, json.Unmarshal(report["cloud_qualification"], &qualification))
	require.Equal(t, "NOT-RUN", qualification.Outcome)
	require.Contains(t, strings.Join(qualification.Causes, ";"), "local")
}

func TestLeanDrainEmptyBoundedGeneration(t *testing.T) {
	for _, which := range []string{"before receipt", "after restore", "wrong generation", "missing generation"} {
		t.Run(which, func(t *testing.T) {
			m := leanFixture(t)
			p := &m.Phases[30]
			switch which {
			case "before receipt":
				p.DrainEmpty.At = p.Run.Events[0].FinishedAt.Add(-time.Nanosecond)
			case "after restore":
				p.DrainEmpty.At = p.Restored.At.Add(time.Nanosecond)
			case "wrong generation":
				require.NoError(t, json.Unmarshal([]byte(`{"generation":"obsolete"}`), p.DrainEmpty))
			case "missing generation":
				require.NoError(t, json.Unmarshal([]byte(`{"generation":""}`), p.DrainEmpty))
			}
			r := judgeLean(m)
			require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
			require.Contains(t, leanCauses(r), "empty proof")
		})
	}
}

func TestLeanAdmissionLiveWindow(t *testing.T) {
	for _, which := range []string{"before media", "after media", "reversed", "clock straddle"} {
		t.Run(which, func(t *testing.T) {
			m := leanFixture(t)
			p := &m.Phases[32]
			p.Admission.Before.At = p.Run.StartedAt.Add(10 * time.Second)
			p.Admission.After.At = p.Run.StartedAt.Add(11 * time.Second)
			switch which {
			case "before media":
				p.Admission.Before.At = p.Run.StartedAt.Add(-time.Second)
			case "after media":
				p.Admission.After.At = p.Run.StartedAt.Add(p.Run.Duration + time.Second)
			case "reversed":
				p.Admission.After.At = p.Admission.Before.At.Add(-time.Second)
			case "clock straddle":
				p.Admission.Before.At = p.Run.StartedAt.Add(time.Millisecond)
			}
			r := judgeLean(m)
			require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
			require.Contains(t, leanCauses(r), "admission")
		})
	}
}

func TestLeanClockFreeContinuityFailures(t *testing.T) {
	for _, which := range []string{"gap", "loss", "forged mapped exemption"} {
		t.Run(which, func(t *testing.T) {
			m := leanFixture(t)
			c := m.Phases[0].Run.Processes[0].Calls[0].Call
			c.Distributed.ClockError = "clock step"
			switch which {
			case "gap":
				c.Tracks[0].MediaGap = 2100 * time.Millisecond
				c.Tracks[0].MediaGapEndedAt = 7 * time.Second
			default:
				d := &c.Recording.Delivery.Audio
				d.Returned--
				d.Lost = 1
				d.LostAt = []time.Duration{time.Second}
				if which == "forged mapped exemption" {
					c.Moves[0].Distributed.Start.Earliest = c.StartedAt
					c.Moves[0].Distributed.Start.Latest = c.StartedAt
				}
			}
			r := judgeLean(m)
			require.Equal(t, "fail", r.Outcome, leanCauses(r))
			require.Contains(t, leanCauses(r), map[bool]string{true: "gap", false: "loss"}[which == "gap"])
		})
	}
	// Loss inside an arm/receipt bracket without valid clocks is unresolved.
	m := leanFixture(t)
	c := m.Phases[0].Run.Processes[0].Calls[0].Call
	c.Distributed.ClockError = "clock step"
	c.Recording.Delivery.Audio = callharness.ContentDelivery{Sent: 1500, Returned: 1499, Lost: 1, LostAt: []time.Duration{6 * time.Second}}
	r := judgeLean(m)
	require.Equal(t, "inconclusive", r.Outcome, leanCauses(r))
}

func TestLeanCostResourceAndMeasuredSteadyEgress(t *testing.T) {
	for _, which := range []string{"cross category", "zero steady", "wrong steady"} {
		t.Run(which, func(t *testing.T) {
			m, _ := leanPricedFixture(t)
			switch which {
			case "cross category":
				m.Prices.Lines[1].Resource = m.Prices.Lines[0].Resource
				m.Prices.Lines[1].Meter = "other-meter"
			case "zero steady":
				m.Prices.Lines[5].SteadyQuantityPerHour = leanPtr(0.0)
				m.Prices.Lines[5].ZeroReason = "purported free transfer"
			case "wrong steady":
				m.Prices.Lines[5].SteadyQuantityPerHour = leanPtr(99.0)
			}
			r := judgeLean(m)
			require.Equal(t, "NOT-RUN", r.Cost.Status)
			require.Nil(t, r.Cost.Total)
		})
	}
}

func TestLeanCostWindowsExplicit(t *testing.T) {
	_, r := leanPricedFixture(t)
	b, err := json.Marshal(r.Cost)
	require.NoError(t, err)
	var cost map[string]any
	require.NoError(t, json.Unmarshal(b, &cost))
	require.Equal(t, "caller ConnectedAt through HungUpAt; includes time outside service telemetry windows", cost["call_hours_window"])
	require.Equal(t, "per-phase service telemetry WindowSeconds", cost["egress_window"])
}

func TestLeanManifestDuplicateKeys(t *testing.T) {
	for _, input := range []string{
		`{"version":0,"version":1}`, `{"clock":{"drift_ppm":1,"drift_ppm":0}}`,
		`{"phases":[{"id":"old","id":"new"}]}`, `{"version":0,"\u0076ersion":1}`,
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "duplicate.json")
			require.NoError(t, os.WriteFile(path, []byte(input), 0600))
			var m leanManifest
			require.ErrorContains(t, readLeanJSON(path, &m), "duplicate JSON key")
		})
	}
}

func TestLeanShortPhaseMemoryExcluded(t *testing.T) {
	m := leanFixture(t)
	r := judgeLean(m)
	require.Equal(t, "pass", r.Outcome, leanCauses(r))
	require.Equal(t, "insufficient-data", r.Phases[0].Memory[0].Outcome)
	b, err := json.Marshal(r.Phases[0])
	require.NoError(t, err)
	var phase map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &phase))
	var gate leanVerdict
	require.NoError(t, json.Unmarshal(phase["memory_gate"], &gate))
	require.Equal(t, "not-applicable", gate.Outcome)
	require.Contains(t, strings.Join(gate.Causes, ";"), "five minutes")
	require.Contains(t, strings.Join(gate.Causes, ";"), "excluded")
}

func TestLeanPlanFalseRefused(t *testing.T) {
	require.ErrorContains(t, runArgs([]string{"-lean-plan=false", "-capacity", "-workers=2"}), "lean-plan requires true")
	m := leanFixture(t)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(path, b, 0600))
	require.ErrorContains(t, runArgs([]string{"-lean-plan=false", "-lean-capacity=" + path, "-output=" + filepath.Join(t.TempDir(), "report.json")}), "lean-plan requires true")
}
