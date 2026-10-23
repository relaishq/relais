// Package rediscommand classifies Redis commands shared by instrumentation.
package rediscommand

import (
	"strings"

	"github.com/redis/go-redis/v9"
)

// IsHandshake identifies connection setup excluded from logical store work and
// move diagnostics. Transport counters still include these commands.
func IsHandshake(cmd redis.Cmder) bool {
	switch cmd.Name() {
	case "hello", "auth", "select":
		return true
	case "client":
		args := cmd.Args()
		if len(args) > 1 {
			subcommand, ok := args[1].(string)
			return ok && strings.EqualFold(subcommand, "setinfo")
		}
	}
	return false
}
