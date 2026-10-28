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
	"strings"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

//go:embed judge-src/*.go
var sources embed.FS

type PinnedJudge struct {
	Commit      string
	SHA256      string
	Binary      string
	Diagnostics io.Writer
}

// BuildPinned archives the exact local commit without writing the checkout or
// .git. Builds are offline and belong before the media window. output must be a
// fresh owned directory; the returned binary's bytes are checked on every call.
func BuildPinned(ctx context.Context, checkout, commit, goExecutable, output string, diagnostics ...io.Writer) (PinnedJudge, error) {
	var pinned PinnedJudge
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(commit) {
		return pinned, errors.New("invalid-judge-pin")
	}
	if err := verifyCompiler(ctx, goExecutable); err != nil {
		return pinned, err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return pinned, err
	}
	source := filepath.Join(output, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		return pinned, err
	}
	archive := exec.CommandContext(ctx, "git", "archive", "--format=tar", commit)
	archive.Dir = checkout
	archive.Env = operatorcaller.PublicEnvironment(os.Environ())
	var archiveErrors bytes.Buffer
	archive.Stderr = &archiveErrors
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return pinned, err
	}
	if err = archive.Start(); err != nil {
		return pinned, err
	}
	extractErr := extract(tar.NewReader(pipe), source)
	if extractErr != nil {
		_ = archive.Process.Kill()
	}
	waitErr := archive.Wait()
	if err = errors.Join(extractErr, waitErr); err != nil {
		printDiagnostics(diagnostics, "judge-archive", nil, archiveErrors.Bytes())
		return pinned, err
	}
	dest := filepath.Join(source, "cmd", "operator-media-judge")
	if err = os.MkdirAll(dest, 0700); err != nil {
		return pinned, err
	}
	entries, err := sources.ReadDir("judge-src")
	if err != nil {
		return pinned, err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, e := sources.ReadFile("judge-src/" + entry.Name())
		if e != nil {
			return pinned, e
		}
		if e = os.WriteFile(filepath.Join(dest, entry.Name()), data, 0600); e != nil {
			return pinned, e
		}
	}
	decoderDir := filepath.Join(source, "internal", "operatorgates", "observe")
	if err = os.RemoveAll(decoderDir); err != nil {
		return pinned, err
	}
	if err = os.MkdirAll(decoderDir, 0700); err != nil {
		return pinned, err
	}
	decoderFiles, err := observe.DecoderSources.ReadDir(".")
	if err != nil {
		return pinned, err
	}
	for _, entry := range decoderFiles {
		data, e := observe.DecoderSources.ReadFile(entry.Name())
		if e != nil {
			return pinned, e
		}
		if e = os.WriteFile(filepath.Join(decoderDir, entry.Name()), data, 0600); e != nil {
			return pinned, e
		}
	}
	binary := filepath.Join(output, "media-judge")
	cmd := exec.CommandContext(ctx, goExecutable, "build", "-trimpath", "-o", binary, "./cmd/operator-media-judge")
	cmd.Dir = source
	cmd.Env = buildEnvironment()
	var buildErrors bytes.Buffer
	cmd.Stderr = &buildErrors
	if out, e := cmd.Output(); e != nil {
		printDiagnostics(diagnostics, "judge-build", out, buildErrors.Bytes())
		return pinned, fmt.Errorf("pinned-judge-build: %w", e)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return pinned, err
	}
	sum := sha256.Sum256(data)
	pinned = PinnedJudge{Commit: commit, SHA256: hex.EncodeToString(sum[:]), Binary: binary}
	if len(diagnostics) > 0 {
		pinned.Diagnostics = diagnostics[0]
	}
	return pinned, nil
}
func extract(reader *tar.Reader, root string) error {
	var total int64
	for {
		h, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !filepath.IsLocal(name) || filepath.Base(name) == ".env" || h.Size < 0 || h.Size > 64<<20 {
			return errors.New("invalid-judge-archive")
		}
		if h.Size > (512<<20)-total {
			return errors.New("judge-archive-size")
		}
		total += h.Size
		dest := filepath.Join(root, name)
		switch h.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			if err = os.MkdirAll(dest, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				return err
			}
			f, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, e = io.Copy(f, reader)
			closeErr := f.Close()
			if err = errors.Join(e, closeErr); err != nil {
				return err
			}
		default:
			return errors.New("judge-archive-type")
		}
	}
}

const pinnedGoVersion = "go1.26.9"

func verifyCompiler(ctx context.Context, executable string) error {
	cmd := exec.CommandContext(ctx, executable, "env", "GOVERSION")
	cmd.Env = buildEnvironment()
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != pinnedGoVersion {
		return errors.New("judge-compiler-version")
	}
	return nil
}
func buildEnvironment() []string {
	var env []string
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		switch key {
		case "PATH", "HOME", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE":
			env = append(env, e)
		}
	}
	return append(env, "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOPROXY=off", "GOSUMDB=off", "GOPRIVATE=", "GONOPROXY=none", "GONOSUMDB=none", "GOFLAGS=-mod=readonly -buildvcs=false", "CGO_ENABLED=0")
}
func (p PinnedJudge) Judge(ctx context.Context, pool Pool) (Summary, error) {
	var out Summary
	if p.Commit == "" || pool.SourceCommit != p.Commit {
		return out, errors.New("judge-source-differs")
	}
	input, err := json.Marshal(pool)
	if err != nil {
		return out, errors.New("judge-input-size")
	}
	result, stderr, err := p.invoke(ctx, input, "")
	if err != nil {
		return out, err
	}
	summary, err := DecodeSummary(result)
	if err != nil {
		printDiagnostics([]io.Writer{p.Diagnostics}, "media-judge-output", result, stderr)
	}
	return summary, err
}

// Calibrate invokes the same hash-verified release judge's generator entry.
// Its operator projection is decoded by the rehearsal, not as product JSON.
func (p PinnedJudge) Calibrate(ctx context.Context, raw string) (json.RawMessage, error) {
	result, _, err := p.invoke(ctx, []byte(raw), "--calibration")
	return result, err
}

func (p PinnedJudge) invoke(ctx context.Context, input []byte, mode string) ([]byte, []byte, error) {
	data, err := os.ReadFile(p.Binary)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != p.SHA256 {
		return nil, nil, errors.New("judge-binary-differs")
	}
	if len(input) > operator.MaxBlobBytes {
		return nil, nil, errors.New("judge-input-size")
	}
	dir, err := os.MkdirTemp("", "relais-media-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	name := filepath.Join(dir, "input.json")
	if err = os.WriteFile(name, input, 0600); err != nil {
		return nil, nil, err
	}
	args := []string{name}
	if mode != "" {
		args = []string{mode, name}
	}
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	cmd.Env = operatorcaller.PublicEnvironment(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	result, err := cmd.Output()
	if err != nil {
		printDiagnostics([]io.Writer{p.Diagnostics}, "media-judge", result, stderr.Bytes())
		return nil, nil, errors.New("judge-incomplete")
	}
	return result, stderr.Bytes(), nil
}

// Outputs come only from credential-stripped public source builds and the
// fixed judge entry. The caller supplies its topology redactor. Incomplete or
// oversized streams are suppressed rather than printing a fragment of a secret.
func printDiagnostics(writers []io.Writer, label string, stdout, stderr []byte) {
	for _, w := range writers {
		if w == nil {
			continue
		}
		if len(stdout) > operator.MaxHostOutputBytes || len(stderr) > operator.MaxHostOutputBytes {
			_, _ = fmt.Fprintf(w, "step %s failed\nstdout/stderr: incomplete output suppressed\n", label)
		} else {
			_, _ = fmt.Fprintf(w, "step %s failed\nstdout:\n%s\nstderr:\n%s\n", label, stdout, stderr)
		}
	}
}
