package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/artifacts"
	"github.com/relaishq/relais/internal/operatorrehearsal"
)

func rehearse(args []string) int {
	return rehearseWithReadiness(args, operatorgates.CheckReadiness, os.Stderr)
}

func rehearseWithReadiness(args []string, check func() error, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("rehearse", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	checkout := flags.String("checkout", ".", "public source checkout")
	assets := flags.String("assets", "runtime/operator-releases", "verified release directory")
	output := flags.String("output", "runtime/operator-report", "sanitized output directory")
	compiler := flags.String("go", "go", "Go 1.26.9 executable")
	commit := flags.String("commit", "", "full tested commit (PR merge ref when applicable)")
	head := flags.String("head-commit", "", "PR head commit, separately from tested merge commit")
	event := flags.String("event", "local", "workflow event")
	prepare := flags.Bool("prepare-only", false, "read and verify fixed releases, then exit")
	prepared := flags.Bool("prepared", false, "use releases from the separate release-read stage")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 2
	}
	if err := check(); err != nil {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	token := os.Getenv("RELAIS_RELEASE_TOKEN")
	_ = os.Unsetenv("RELAIS_RELEASE_TOKEN")
	abs := func(s string) string {
		v, e := filepath.Abs(s)
		if e != nil {
			return ""
		}
		return v
	}
	if *prepare || !*prepared {
		if e := operatorrehearsal.PrepareReleases(ctx, abs(*assets), token); e != nil {
			r := operatorrehearsal.Report{SchemaVersion: 1, Commit: *commit, HeadCommit: *head, Event: *event, Cleanup: true, Errors: []string{"release-read-failed"}}
			var refusal *artifacts.Refusal
			if errors.As(e, &refusal) {
				r.Errors = append(r.Errors, refusal.Code)
			}
			if os.MkdirAll(abs(*output), 0700) == nil {
				_ = r.Save(filepath.Join(abs(*output), "report.json"))
			}
			fmt.Fprintln(diagnostics, "release-read-failed")
			_ = r.Write(diagnostics)
			return 1
		}
	}
	if *prepare {
		return 0
	}
	executable, e := exec.LookPath(*compiler)
	if e != nil {
		fmt.Fprintln(diagnostics, "compiler-unavailable")
		return 1
	}
	r, e := operatorrehearsal.Rehearse(ctx, operatorrehearsal.RehearseConfig{Checkout: abs(*checkout), Assets: abs(*assets), Output: abs(*output), GoExecutable: abs(executable), Commit: *commit, HeadCommit: *head, Event: *event}, diagnostics)
	if e != nil {
		fmt.Fprintln(diagnostics, "rehearsal-incomplete")
	}
	if r.Write(os.Stdout) != nil || e != nil || r.Verdict != "passed" {
		return 1
	}
	return 0
}
