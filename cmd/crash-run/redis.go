package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-redis/redis/v8"
	"github.com/relais/internal/nettopology"
	"github.com/relais/internal/processrun"
)

var redisSecretPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The loopback command stays unchanged. Namespaced Redis retains protected
// mode and requires a per-run password, kept out of argv and failure artifacts.
func redisCommand(topology *nettopology.Topology, binary, port, dir, password string) ([]string, func(), error) {
	args := []string{binary}
	removeConfig := func() {}
	if err := processrun.ValidateRedis(net.JoinHostPort(redisHost(topology), port)); err != nil {
		return nil, removeConfig, err
	}
	if topology != nil {
		if !redisSecretPattern.MatchString(password) {
			return nil, removeConfig, errors.New("namespaced Redis requires a random per-run password")
		}
		// Linux netns runs as root. A root-owned 0700 directory under the
		// system's sticky /tmp prevents the checkout owner swapping the file.
		configDir, err := os.MkdirTemp("/tmp", "relais-redis-auth-*")
		if err != nil {
			return nil, removeConfig, err
		}
		removeConfig = func() { _ = os.RemoveAll(configDir) }
		config, err := os.OpenFile(filepath.Join(configDir, "redis.conf"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			removeConfig()
			return nil, removeConfig, err
		}
		_, writeErr := config.WriteString("requirepass " + password + "\n")
		if err := errors.Join(writeErr, config.Close()); err != nil {
			removeConfig()
			return nil, removeConfig, err
		}
		args = append(args, config.Name())
	}
	args = append(args, "--bind", redisHost(topology), "--port", port, "--save", "", "--appendonly", "no", "--dir", dir)
	return args, removeConfig, nil
}

func pingRedis(ctx context.Context, addr, password string) error {
	if err := processrun.ValidateRedis(addr); err != nil {
		return err
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Password: password, MaxRetries: -1})
	defer func() { _ = client.Close() }()
	return client.Ping(ctx).Err()
}

func trialFailure(number int, mode, dir string, err error) error {
	return fmt.Errorf("TRIAL %d MODE %s FAILED: %w (logs: %s)", number, mode, err, dir)
}
