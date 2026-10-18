//go:build !darwin && !linux

package redistestcluster

import (
	"os"

	"github.com/relaishq/relais/internal/processidentity"
)

func registryOwner(os.FileInfo) error { return processidentity.ErrUnsupported }
