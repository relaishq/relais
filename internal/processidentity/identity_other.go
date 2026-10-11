//go:build !darwin && !linux

package processidentity

import "errors"

func inspect(int) (string, bool, error) {
	return "", false, errors.New("standby process identity requires Linux or macOS")
}
