package main

import (
	"flag"
	"fmt"
)

func validateWorkerRestartFlags(flags *flag.FlagSet) error {
	var err error
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "worker-restart", "runs", "after", "bin", "redis", "redis-server":
		default:
			err = fmt.Errorf("worker-restart cannot combine with -%s", f.Name)
		}
	})
	return err
}
