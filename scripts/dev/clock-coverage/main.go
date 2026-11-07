// clock-coverage evaluates stored rehearsal evidence without running a rehearsal
// or invoking the media judge. Its output contains indexes and fixed codes only.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func evaluate(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("clock-coverage", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	reportPath := flags.String("report", "", "stored report JSON")
	poolPath := flags.String("pool", "", "optional retained pool JSON (same clock domain)")
	frontend := flags.String("frontend-slot", "", "inventory-derived frontend slot")
	control := flags.String("control-slot", "", "inventory-derived control slot")
	head := flags.Bool("head-caller", false, "inspect the separate head caller sequence")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *reportPath == "" {
		return fmt.Errorf("-report is required")
	}
	raw, err := os.ReadFile(*reportPath)
	if err != nil {
		return fmt.Errorf("report unavailable")
	}
	var report storedReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("invalid report JSON")
	}
	if *head {
		if report.HeadCaller == nil {
			return fmt.Errorf("head caller evidence unavailable")
		}
		report.CallerTiming = report.HeadCaller.CallerTiming
		report.Coverage = report.HeadCaller.Coverage
	}
	e := report.Coverage
	historical := e == nil
	var historicalPool *operatorevidence.Pool
	uncertaintyNS := int64(0)
	notes := []string{}
	if e == nil {
		e = &clocks.CoverageEvidence{Version: 2, Bounds: report.Bounds, Attempts: storedAttempts(report.Clocks)}
		notes = append(notes, "coverage-reconstructed-from-tracking-rows")
	}
	if *frontend != "" {
		e.FrontendSlot = *frontend
	}
	if *control != "" {
		e.ControlSlot = *control
	}
	if *poolPath != "" {
		data, err := os.ReadFile(*poolPath)
		if err != nil {
			return fmt.Errorf("pool unavailable")
		}
		var envelope struct {
			Pool json.RawMessage `json:"pool"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("invalid pool JSON")
		}
		if len(envelope.Pool) > 0 {
			data = envelope.Pool
		}
		var pool operatorevidence.Pool
		if err := json.Unmarshal(data, &pool); err != nil {
			return fmt.Errorf("invalid pool JSON")
		}
		if historical || len(e.Windows) == 0 {
			e.Windows = operatorevidence.CoverageWindows(pool)
			historicalPool = &pool
		}
	}
	if len(e.Windows) == 0 {
		notes = append(notes, "event-inputs-unavailable")
	}
	e.Evaluate()
	results := e.Results
	if historical {
		if historicalPool != nil {
			var err error
			e.Windows, uncertaintyNS, err = alignHistoricalWindows(report, *historicalPool)
			if err != nil {
				return err
			}
			notes = append(notes, "clock-domain-aligned-from-report-event-milliseconds")
		}
		results = observe.EvaluateClockCoverageWithoutKillBrackets(historicalTimelines(report.Clocks), e.FrontendSlot, e.ControlSlot, e.Bounds.FrontendControlBoundNS, e.Bounds.FrontendWorkerBoundNS, e.Windows, uncertaintyNS)
		for _, w := range e.Windows {
			if eventKind(w.Kind) == "kill" {
				notes = append(notes, "kill-bracket-unavailable")
				break
			}
		}
	}
	counts := map[string]int{}
	covered := 0
	for _, r := range results {
		if len(r.Causes) == 0 {
			covered++
		}
		for _, c := range r.Causes {
			counts[c]++
		}
	}
	details := make([]eventResult, 0, len(results))
	for _, r := range results {
		d := eventResult{CoverageResult: r}
		for _, w := range e.Windows {
			if w.Phase == r.Phase && w.Event == r.Event {
				d.Kind = eventKind(w.Kind)
				d.Time = windowTime(w.StartUnixNS)
				break
			}
		}
		if historical && d.Kind == "kill" {
			d.KillBracketRule = "unavailable"
		}
		details = append(details, d)
	}
	return json.NewEncoder(out).Encode(struct {
		Version                int            `json:"version"`
		Attempts               int            `json:"attempts"`
		Events                 int            `json:"events"`
		Covered                int            `json:"covered"`
		DrainComplete          bool           `json:"drainComplete"`
		Causes                 map[string]int `json:"wouldBeCauses"`
		Results                []eventResult  `json:"results"`
		TimestampUncertaintyNS int64          `json:"timestampUncertaintyNs"`
		Notes                  []string       `json:"notes"`
	}{2, len(e.Attempts), len(results), covered, e.DrainComplete, counts, details, uncertaintyNS, notes})
}
func main() {
	if err := evaluate(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func storedAttempts(rows []clocks.Evidence) []clocks.CoverageAttempt {
	out := make([]clocks.CoverageAttempt, 0, len(rows))
	for _, r := range rows {
		out = append(out, clocks.NewCoverageAttempt(r))
	}
	return out
}

type eventResult struct {
	observe.CoverageResult
	Kind            string `json:"kind"`
	Time            string `json:"time"`
	KillBracketRule string `json:"killBracketRule,omitempty"`
}
