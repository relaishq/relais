// Package redisendpoint validates Redis endpoints before network I/O.
package redisendpoint

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Validate permits managed Redis ports, while protecting a local developer's Redis.
// Hostnames other than localhost count as remote. Validation never resolves DNS.
func Validate(addr string) error {
	return validate(addr, false)
}

// ValidateDev always refuses 6379, on every host, regardless of the local opt-in.
// Tests, demos and development drivers must use this stricter check.
func ValidateDev(addr string) error {
	return validate(addr, true)
}

func validate(addr string, dev bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" || strings.ContainsAny(host, "/@?# \t\r\n") {
		return errors.New("an explicit Redis host:port is required")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("redis requires a numeric port between 1 and 65535")
	}
	local := strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsUnspecified() {
			return errors.New("Redis requires a specific host, not an unspecified address")
		}
		local = ip.IsLoopback()
	}
	if dev {
		if number == 6379 {
			return errors.New("port 6379 is reserved; development and test tools require a dedicated Redis instance")
		}
		return nil
	}
	allow, err := envSwitch("RELAIS_REDIS_ALLOW_LOCAL_6379")
	if err != nil {
		return err
	}
	if number == 6379 && local && !allow {
		return errors.New("local Redis port 6379 is reserved to protect the developer's own Redis; use a dedicated instance or explicitly opt in with RELAIS_REDIS_ALLOW_LOCAL_6379=1")
	}
	return nil
}

func envSwitch(name string) (bool, error) {
	switch os.Getenv(name) {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New(name + " must be 0 or 1")
	}
}
