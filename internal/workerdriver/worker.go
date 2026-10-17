// Package workerdriver selects the worker executable for process drivers.
package workerdriver

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type Options struct{ Binary, Agent string }

func (o *Options) Flags(fs *flag.FlagSet) {
	fs.StringVar(&o.Binary, "worker-bin", "", "worker executable path (default relais-worker in -bin)")
	fs.StringVar(&o.Agent, "agent", "", "worker agent name (default selected worker's default)")
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
	result := append([]string{o.Path(bin)}, args...)
	if o.Agent != "" {
		result = append(result, "-agent", o.Agent)
	}
	return result
}
func (o Options) Check(bin string) error {
	path := o.Path(bin)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return fmt.Errorf("missing executable worker %s (make build or set -worker-bin)", path)
	}
	return nil
}
