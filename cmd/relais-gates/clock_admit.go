package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/operator"
)

// clockAdmitPlan is everything admission needs that is known before measurement.
// Scenario is the unbound plan; its digest must be its own plan identity.
type clockAdmitPlan struct {
	RunID              string            `json:"runId"`
	FrontendSlot       string            `json:"frontendSlot"`
	ControlSlot        string            `json:"controlSlot"`
	CallerLeapMode     string            `json:"callerLeapMode"`
	HelpersUntilUnixMS string            `json:"helpersUntilUnixMs"`
	Addresses          map[string]string `json:"addresses"`
	Scenario           operator.Scenario `json:"scenario"`
}

var clockAdmitRun = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// A lease read is at most 256000 decoded bytes; base64 of incompressible gzip stays below this.
const clockAdmitHostOutputLimit = 512 << 10

// After a host command exits or is cancelled, its pipes get this long to close.
const clockAdmitHostWaitDelay = time.Second

type clockAdmitClock struct {
	now   func() time.Time
	pause func(context.Context, time.Duration) error
}

// Exit 0: admitted, receipt and binding written. Exit 1: admission ran and
// refused. Exit 2: refused before any host command. Stderr carries one fixed cause.
func clockAdmit(args []string, diagnostics io.Writer, clock clockAdmitClock) int {
	flags := flag.NewFlagSet("clock-admit", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	planFile := flags.String("plan", "", "admission plan JSON file")
	policyFile := flags.String("policy", "", "launcher clock policy JSON file")
	program := flags.String("host-command", "", "absolute host command executable")
	output := flags.String("output", "", "existing directory for admission artifacts")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *planFile == "" || *policyFile == "" || *output == "" || !filepath.IsAbs(*program) {
		fmt.Fprintln(diagnostics, "usage: relais-gates clock-admit -plan FILE -policy FILE -host-command PROGRAM -output DIR")
		return 2
	}
	var plan clockAdmitPlan
	var policy clocks.SelectionPolicy
	directory, err := os.Stat(*output)
	if err != nil || !directory.IsDir() || readClockSelection(*planFile, &plan) != nil || readClockSelection(*policyFile, &policy) != nil {
		fmt.Fprintln(diagnostics, "clock-admit-input")
		return 2
	}
	until, err := strconv.ParseInt(plan.HelpersUntilUnixMS, 10, 64)
	valid := err == nil && clockAdmitRun.MatchString(plan.RunID) && slices.Contains(inventory.Slots, plan.FrontendSlot) && slices.Contains(inventory.Slots, plan.ControlSlot) && (plan.CallerLeapMode == "smeared" || plan.CallerLeapMode == "unsmeared") && len(plan.Addresses) == len(inventory.Slots)
	for _, slot := range inventory.Slots {
		address, err := netip.ParseAddr(plan.Addresses[slot])
		valid = valid && err == nil && address.Is4()
	}
	if !valid {
		fmt.Fprintln(diagnostics, "clock-admit-input")
		return 2
	}
	// Both rules belong to the shared admission and binding code. They run here
	// too so that a refusal costs no host command.
	if err := operatorrehearsal.CheckClockAdmissionPolicy(policy); err != nil {
		fmt.Fprintln(diagnostics, err)
		return 2
	}
	if _, err := operator.CheckPlan(plan.Scenario); err != nil {
		fmt.Fprintln(diagnostics, err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	status := operatorrehearsal.AdmitClock(ctx, operatorrehearsal.ClockAdmission{
		RunID: plan.RunID, FrontendSlot: plan.FrontendSlot, ControlSlot: plan.ControlSlot, CallerLeapMode: plan.CallerLeapMode,
		Policy: policy, Addresses: plan.Addresses, HelpersUntil: time.UnixMilli(until), Output: *output,
		Host: clockAdmitHost(*program), Now: clock.now, Pause: clock.pause,
	})
	if status.Cause != "" {
		fmt.Fprintln(diagnostics, status.Cause)
		return 1
	}
	binding, _, err := operatorrehearsal.BindClockAdmission(*output, status, plan.Scenario)
	if err != nil {
		cause := err.Error()
		if errors.Is(err, fs.ErrNotExist) {
			// The admitted receipt or its evidence is no longer in the output directory.
			cause = "clock-binding-missing"
		} else if !strings.HasPrefix(cause, "clock-binding-") {
			cause = "clock-binding-invalid"
		}
		fmt.Fprintln(diagnostics, cause)
		return 1
	}
	raw, err := json.Marshal(binding)
	if err != nil || os.WriteFile(filepath.Join(*output, "clock-binding.json"), raw, 0600) != nil {
		fmt.Fprintln(diagnostics, "clock-selection-artifact")
		return 1
	}
	return 0
}

// clockAdmitHost runs `PROGRAM SLOT run|detach ARG...`. Host stderr is dropped:
// only fixed causes reach this command's diagnostics.
func clockAdmitHost(program string) operatorrehearsal.ClockHostCommand {
	failed := errors.New("clock-selection-read")
	return func(ctx context.Context, slot string, detach bool, input []byte, args ...string) (string, error) {
		mode := "run"
		if detach {
			mode = "detach"
		}
		cmd := exec.CommandContext(ctx, program, append([]string{slot, mode}, args...)...)
		cmd.Stdin = bytes.NewReader(input)
		// A child that keeps a pipe open cannot hold Run past exit or cancellation.
		cmd.WaitDelay = clockAdmitHostWaitDelay
		var out clockAdmitOutput
		if !detach {
			// A detached helper gets no pipe: it may inherit stdout for its lifetime.
			cmd.Stdout = &out
		}
		if cmd.Run() != nil {
			return "", failed
		}
		return string(out), nil
	}
}

// clockAdmitOutput refuses host output above the limit instead of buffering it.
type clockAdmitOutput []byte

func (o *clockAdmitOutput) Write(p []byte) (int, error) {
	if len(*o)+len(p) > clockAdmitHostOutputLimit {
		return 0, errors.New("clock-selection-read")
	}
	*o = append(*o, p...)
	return len(p), nil
}
