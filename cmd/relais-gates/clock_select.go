package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

func clockSelect(args []string, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("clock-select", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	evidence := flags.String("evidence", "", "admission evidence JSON file")
	policy := flags.String("policy", "", "launcher policy JSON file")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *evidence == "" || *policy == "" {
		return 2
	}
	var input clocks.SelectionEvidence
	var p clocks.SelectionPolicy
	if readClockSelection(*evidence, &input) != nil || readClockSelection(*policy, &p) != nil {
		fmt.Fprintln(diagnostics, "clock-select-input")
		return 1
	}
	selected, err := clocks.Select(input, p)
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	if json.NewEncoder(out).Encode(selected) != nil {
		return 1
	}
	return 0
}

func readClockSelection(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const limit = operator.ClockArtifactLimit
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	// Whitespace/key order are accepted; receipt bytes are always recomputed.
	return observe.DecodeClosedLimit(b, v, nil, limit)
}
