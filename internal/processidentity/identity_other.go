//go:build !darwin && !linux

package processidentity

import "context"

func inspect(int) (string, bool, error)             { return "", false, ErrUnsupported }
func fencePlatform(context.Context, Identity) error { return ErrUnsupported }

func gonePlatform(context.Context, Identity) (bool, error) { return false, ErrUnsupported }
