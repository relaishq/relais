//go:build darwin || linux

package redistestcluster

import (
	"errors"
	"os"
	"syscall"
)

func registryOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("server ownership records must belong to the harness user")
	}
	return nil
}
