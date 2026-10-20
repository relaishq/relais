// Package workerdriver selects the worker executable for process drivers.
package workerdriver

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Options struct {
	Binary, Agent, Subcommand string
	Args                      []string
}

func (o *Options) Flags(fs *flag.FlagSet) {
	fs.StringVar(&o.Subcommand, "worker-subcommand", "", "optional worker subcommand")
	fs.Func("worker-arg", "repeatable worker argument", func(value string) error { o.Args = append(o.Args, value); return nil })
	fs.StringVar(&o.Binary, "worker-bin", "", "worker executable path (requires -agent; default relais-worker in -bin)")
	fs.StringVar(&o.Agent, "agent", "", "worker agent name (required with -worker-bin; default stock worker's default)")
}
func (o Options) Path(bin string) string {
	if o.Binary != "" {
		path, err := filepath.Abs(o.Binary)
		if err == nil {
			return path
		}
		return o.Binary
	}
	return filepath.Join(bin, "relais-worker")
}

// Command preserves the stock command exactly when both overrides are unset.
func (o Options) Command(bin string, args ...string) []string {
	result := []string{o.Path(bin)}
	if o.Subcommand != "" {
		result = append(result, o.Subcommand)
	}
	result = append(result, args...)
	if o.Agent != "" {
		result = append(result, "-agent", o.Agent)
	}
	return append(result, o.Args...)
}

// IsCustom classifies application semantics by agent identity, never by binary path.
func IsCustom(name string) bool { return name != "" && name != "echo" && name != "demo" }

func (o Options) Validate() error {
	if o.Binary != "" && strings.TrimSpace(o.Agent) == "" {
		return fmt.Errorf("-agent is required with -worker-bin (use echo or demo explicitly for stock semantics)")
	}
	return nil
}
func (o Options) Check(bin string) error {
	if err := o.Validate(); err != nil {
		return err
	}
	path := o.Path(bin)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return fmt.Errorf("missing executable worker %s (make build or set -worker-bin)", path)
	}
	return nil
}
