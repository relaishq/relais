// Package redisendpoint validates dedicated Redis endpoints before network I/O.
package redisendpoint

import (
	"errors"
	"net"
	"strconv"
)

func Validate(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return errors.New("an explicit Redis host:port is required")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("redis requires a numeric port between 1 and 65535")
	}
	if number == 6379 {
		return errors.New("port 6379 is reserved; use a dedicated Redis instance")
	}
	return nil
}
