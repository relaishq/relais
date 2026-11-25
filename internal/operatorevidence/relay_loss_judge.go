package operatorevidence

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

//go:embed relayjudge/*.go
var relayJudgeSources embed.FS

// RelayLossJudge cannot be used through the A/B/M summary decoder.
type RelayLossJudge struct{ pinned PinnedJudge }

func (p RelayLossJudge) Identity() operator.Identity {
	return operator.Identity{SourceCommit: p.pinned.Commit, SHA256: p.pinned.SHA256}
}

// BindScenario changes only an explicitly selected part R scenario. Call this
// before clock receipt binding; the ordinary scenario digest includes the R
// executable hash through its existing Judge field.
func (p RelayLossJudge) BindScenario(s operator.Scenario) (operator.Scenario, error) {
	if !s.RelayTwo || !slices.Equal(s.Gates, []operator.Gate{operator.RelayHostLoss}) || s.Caller.SourceCommit != p.pinned.Commit || s.ClockReceiptSHA256 != "" {
		return operator.Scenario{}, errors.New("relay-loss-judge-scenario")
	}
	s.Judge = p.Identity()
	return operator.BuildScenarioIdentity(s, "")
}

// BuildRelayLossPinned follows BuildPinned's archive + current embedded judge
// model, but overlays only the R directory and a fixed entry point. In
// particular it never replaces observe or copies judge-src. Dependencies come
// from commit, so a release lacking an R dependency fails the offline build.
func BuildRelayLossPinned(ctx context.Context, checkout, commit, goExecutable, output string, diagnostics ...io.Writer) (RelayLossJudge, error) {
	var out RelayLossJudge
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(commit) {
		return out, errors.New("invalid-judge-pin")
	}
	if err := verifyCompiler(ctx, goExecutable); err != nil {
		return out, err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return out, err
	}
	source := filepath.Join(output, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		return out, err
	}
	archive, err := os.CreateTemp(output, "source-*.tar")
	if err != nil {
		return out, err
	}
	defer archive.Close()
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", commit)
	cmd.Dir = checkout
	cmd.Env = operatorcaller.PublicEnvironment(os.Environ())
	cmd.Stdout = archive
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		printDiagnostics(diagnostics, "relay-loss-judge-archive", nil, stderr.Bytes())
		return out, err
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	if err = extract(tar.NewReader(archive), source); err != nil {
		return out, err
	}
	dest := filepath.Join(source, "internal", "operatorevidence", "relayjudge")
	if err = overlayRelayJudge(dest); err != nil {
		return out, err
	}
	entry := filepath.Join(source, "cmd", "operator-relay-loss-judge")
	if err = os.MkdirAll(entry, 0700); err != nil {
		return out, err
	}
	if err = os.WriteFile(filepath.Join(entry, "main.go"), []byte(relayLossMain), 0600); err != nil {
		return out, err
	}
	binary := filepath.Join(output, "relay-loss-judge")
	cmd = exec.CommandContext(ctx, goExecutable, "build", "-trimpath", "-o", binary, "./cmd/operator-relay-loss-judge")
	cmd.Dir = source
	cmd.Env = buildEnvironment()
	stderr.Reset()
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		printDiagnostics(diagnostics, "relay-loss-judge-build", nil, stderr.Bytes())
		return out, fmt.Errorf("relay-loss-judge-build: %w", err)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(raw)
	out.pinned = PinnedJudge{Commit: commit, SHA256: hex.EncodeToString(sum[:]), Binary: binary}
	if len(diagnostics) > 0 {
		out.pinned.Diagnostics = diagnostics[0]
	}
	return out, nil
}

// Replace the archived directory so removed source files cannot enter an R build.
func overlayRelayJudge(dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	entries, err := relayJudgeSources.ReadDir("relayjudge")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, e := relayJudgeSources.ReadFile("relayjudge/" + entry.Name())
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(dest, entry.Name()), raw, 0600); e != nil {
			return e
		}
	}
	return nil
}

const relayLossMain = `package main
import (
 "os"
 "github.com/relaishq/relais/internal/operatorevidence/relayjudge"
)
func main() {if relayjudge.Run(os.Args[1:],os.Stdout)!=nil {os.Exit(1)}}
`

func (p RelayLossJudge) Judge(ctx context.Context, in relayjudge.Input) (relayjudge.Result, error) {
	var result relayjudge.Result
	if in.Scenario.Caller.SourceCommit != p.pinned.Commit || in.Scenario.Judge != p.Identity() {
		return result, errors.New("relay-loss-judge-source-differs")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return result, err
	}
	stdout, _, err := p.pinned.invoke(ctx, raw, "")
	if err != nil {
		return result, err
	}
	err = observe.DecodeClosedLimit(stdout, &result, nil, operator.MaxBlobBytes)
	return result, err
}

// OpenRelayLossJudge uses an already-built, scenario-bound executable. Every
// invocation still verifies its bytes; this does not build during the campaign.
func OpenRelayLossJudge(commit, sha, binary string) RelayLossJudge {
	return RelayLossJudge{pinned: PinnedJudge{Commit: commit, SHA256: sha, Binary: binary}}
}
