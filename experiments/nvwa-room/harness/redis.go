package harness

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/go-redis/redis/v8"
	"github.com/relaishq/relais/internal/redisendpoint"
)

// redisLaunch keeps loopback unchanged. Private Redis keeps protected mode and
// reads its random password from a 0600 config, never from argv or public config.
func redisLaunch(host, port, dir string, authenticated bool) ([]string, string, func(), error) {
	cleanup := func() {}
	if err := redisendpoint.ValidateDev(net.JoinHostPort(host, port)); err != nil {
		return nil, "", cleanup, err
	}
	var args []string
	password := ""
	if authenticated {
		password = randomHex()
		path := filepath.Join(dir, "redis-auth.conf")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, "", cleanup, err
		}
		cleanup = func() { _ = os.Remove(path) }
		_, writeErr := f.WriteString("protected-mode yes\nrequirepass " + password + "\n")
		if err := errors.Join(writeErr, f.Close()); err != nil {
			cleanup()
			return nil, "", cleanup, err
		}
		args = append(args, path)
	}
	args = append(args, "--bind", host, "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
	return args, password, cleanup, nil
}
func pingRedis(ctx context.Context, addr, password string) error {
	c := redis.NewClient(&redis.Options{Addr: addr, Password: password, MaxRetries: -1})
	defer c.Close()
	return c.Ping(ctx).Err()
}
