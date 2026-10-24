// relais-compat is an offline artifact validator and rollout preflight.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/buildinfo"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }
func run(args []string, out io.Writer) int {
	fail := func(err error) int {
		r := &releasecompat.Refusal{Code: "invalid_input", Reason: err.Error()}
		_ = errors.As(err, &r)
		_ = json.NewEncoder(out).Encode(releasecompat.Verdict{Refusal: r})
		return 1
	}
	if len(args) == 1 && args[0] == "-version" {
		if err := buildinfo.PrintVersion(out, "relais-compat"); err != nil {
			return 1
		}
		return 0
	}
	if len(args) == 0 {
		return fail(errors.New("expected generate, check, artifact or preflight"))
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", ".", "repository root (generation/check only)")
	version := flags.String("version", "0.0.0-dev", "release version without v")
	commit := flags.String("commit", "", "full release commit")
	osName := flags.String("os", "any", "archive OS")
	arch := flags.String("arch", "any", "archive architecture")
	output := flags.String("output", "", "generation output (default stdout)")
	manifestPath := flags.String("manifest", "", "artifact manifest path")
	binaries := flags.String("binary-dir", "", "artifact directory containing all six services and relais-compat")
	running := flags.String("running", "", "offline pool inventory")
	target := flags.String("target", "", "target archive manifest")
	operation := flags.String("operation", "", "upgrade or rollback")
	base := flags.String("base", "", "base commit for immutable contract history")
	if err := flags.Parse(args[1:]); err != nil {
		return fail(err)
	}
	if flags.NArg() != 0 {
		return fail(errors.New("unexpected positional arguments"))
	}
	read := func(path string, value any) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
		if err != nil {
			return err
		}
		return releasecompat.Decode(data, value)
	}
	switch args[0] {
	case "generate", "check":
		m, err := releasecompat.Generate(*root, *version, *commit, *osName, *arch)
		if err != nil {
			return fail(err)
		}
		data, err := releasecompat.JSON(m)
		if err != nil {
			return fail(err)
		}
		if args[0] == "generate" {
			if *output != "" {
				if err = os.WriteFile(*output, data, 0644); err != nil {
					return fail(err)
				}
			} else {
				if _, err = out.Write(data); err != nil {
					return 1
				}
			}
			return 0
		}
		path := filepath.Join(*root, "scripts/release/compatibility.json")
		checked, err := os.ReadFile(path)
		if err != nil {
			return fail(err)
		}
		if !bytes.Equal(data, checked) {
			return fail(errors.New("checked-in manifest differs from generated constants/goldens; regenerate and review"))
		}
		if *base != "" {
			cmd := exec.Command("git", "-C", *root, "ls-tree", *base, "--", "scripts/release/compatibility.json")
			listed, e := cmd.Output()
			if e != nil {
				return fail(fmt.Errorf("cannot inspect contract base: %w", e))
			}
			if len(listed) > 0 {
				cmd = exec.Command("git", "-C", *root, "show", *base+":scripts/release/compatibility.json")
				previous, e := cmd.Output()
				if e != nil {
					return fail(e)
				}
				var old releasecompat.Manifest
				if e = releasecompat.Decode(previous, &old); e != nil {
					return fail(e)
				}
				if e = releasecompat.CheckEvolution(old, m); e != nil {
					return fail(e)
				}
			}
		}
		_, err = fmt.Fprintln(out, "compatibility manifest matches constants and goldens")
		if err != nil {
			return 1
		}
		return 0
	case "artifact":
		var m releasecompat.Manifest
		if err := read(*manifestPath, &m); err != nil {
			return fail(err)
		}
		if err := m.Validate(); err != nil {
			return fail(err)
		}
		if m.Version != *version || m.OS != *osName || m.Arch != *arch || m.Commit == "" {
			return fail(&releasecompat.Refusal{Code: "identity_mismatch", Reason: "archive manifest identity/platform differs from pinned artifact"})
		}
		if *binaries == "" {
			return fail(errors.New("binary-dir is required"))
		}
		for _, name := range []string{"relais-relay", "relais-worker", "relais-control", "relais-signal", "relais-load", "relais-demo", "relais-compat"} {
			identity, err := exec.Command(filepath.Join(*binaries, name), "-version").Output()
			if err != nil {
				return fail(fmt.Errorf("read artifact binary identity %s: %w", name, err))
			}
			expected := name + " version=" + m.Version + " commit=" + m.Commit + " date="
			if !strings.HasPrefix(string(identity), expected) {
				return fail(&releasecompat.Refusal{Code: "identity_mismatch", Reason: "binary identity does not match manifest: " + name})
			}
		}
		_, _ = fmt.Fprintln(out, "artifact manifest and binary identities verified")
		return 0
	case "preflight":
		var pool releasecompat.Pool
		var m releasecompat.Manifest
		if err := read(*running, &pool); err != nil {
			return fail(err)
		}
		if err := read(*target, &m); err != nil {
			return fail(err)
		}
		verdict := releasecompat.Preflight(pool, m, *operation)
		if err := json.NewEncoder(out).Encode(verdict); err != nil {
			return 1
		}
		if !verdict.Allowed {
			return 1
		}
		return 0
	default:
		return fail(errors.New("unknown command"))
	}
}
