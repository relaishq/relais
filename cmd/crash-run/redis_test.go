package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relais/internal/nettopology"
	"github.com/stretchr/testify/require"
)

func TestRedisCommandsPreserveLoopbackAndAuthenticateNetns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crash-run-owned")
	require.NoError(t, os.Mkdir(dir, 0700))
	args, cleanup, err := redisCommand(nil, "redis-server", "16379", dir, "")
	require.NoError(t, err)
	defer cleanup()
	require.Equal(t, []string{"redis-server", "--bind", "127.0.0.1", "--port", "16379", "--save", "", "--appendonly", "no", "--dir", dir}, args)
	plan, err := nettopology.NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	topology := &nettopology.Topology{Plan: plan}
	_, _, err = redisCommand(topology, "redis-server", "16379", dir, "")
	require.Error(t, err)
	password := strings.Repeat("ab", 32)
	args, cleanup, err = redisCommand(topology, "redis-server", "16379", dir, password)
	require.NoError(t, err)
	defer cleanup()
	require.NotContains(t, strings.Join(args, " "), password)
	require.Equal(t, filepath.Dir(dir), filepath.Dir(args[1]), "credentials must stay outside uploaded run directories")
	require.Equal(t, []string{"--bind", plan.Roles["store"].Private.String(), "--port", "16379", "--save", "", "--appendonly", "no", "--dir", dir}, args[2:])
	stat, err := os.Stat(args[1])
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), stat.Mode().Perm())
	data, err := os.ReadFile(args[1])
	require.NoError(t, err)
	require.Equal(t, "requirepass "+password+"\n", string(data))
	cleanup()
	_, err = os.Stat(args[1])
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTrialFailureContainsContextAndCause(t *testing.T) {
	cause := errors.New("relay readiness: fatal startup: Redis authentication failed")
	err := trialFailure(1, "redis-cache+pli", "/owned/logs", cause)
	require.ErrorIs(t, err, cause)
	require.ErrorContains(t, err, "TRIAL 1 MODE redis-cache+pli FAILED")
	require.ErrorContains(t, err, cause.Error())
	require.ErrorContains(t, err, "/owned/logs")
}

func TestRedisAuthenticatedPing(t *testing.T) {
	password := os.Getenv("RELAIS_TEST_REDIS_PASSWORD")
	if password == "" {
		t.Skip("set RELAIS_TEST_REDIS_PASSWORD with a dedicated password-protected Redis")
	}
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	require.NotEmpty(t, addr)
	require.NoError(t, pingRedis(context.Background(), addr, password))
	require.ErrorContains(t, pingRedis(context.Background(), addr, ""), "NOAUTH")
}
