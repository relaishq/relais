//go:build darwin || linux

package redistestcluster

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const standaloneGateHelper = "RELAIS_STANDALONE_GATE_HELPER"

func runStandaloneGateHelper(t *testing.T, env map[string]string, path string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOwnedStandaloneRedisGate$", "-test.v")
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "PATH", "GORACE", "RELAIS_TEST_REDIS_REQUIRE", "RELAIS_TEST_REDIS_CLUSTER", "RELAIS_TEST_REDIS_ADDR", "RELAIS_TEST_ARTIFACT_DIR", standaloneGateHelper:
		default:
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, standaloneGateHelper+"=1", "PATH="+path, "GORACE=atexit_sleep_ms=0", "RELAIS_TEST_ARTIFACT_DIR="+t.TempDir())
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	data, err := cmd.CombinedOutput()
	require.NoError(t, ctx.Err(), "fixture helper timed out: %s", data)
	return string(data), err
}

func TestOwnedStandaloneRedisGate(t *testing.T) {
	if os.Getenv(standaloneGateHelper) == "1" {
		Standalone(t)
		t.Fatal("gate helper must skip or reject absent prerequisites before starting Redis")
	}
	t.Run("unset-skips-without-tools", func(t *testing.T) {
		output, err := runStandaloneGateHelper(t, nil, "")
		require.NoError(t, err, output)
		require.Contains(t, output, "--- SKIP:")
		require.Contains(t, output, "set RELAIS_TEST_REDIS_REQUIRE=1 or RELAIS_TEST_REDIS_CLUSTER=1")
	})
	for _, gate := range []string{"RELAIS_TEST_REDIS_REQUIRE", "RELAIS_TEST_REDIS_CLUSTER"} {
		t.Run(gate+"-requires-redis", func(t *testing.T) {
			output, err := runStandaloneGateHelper(t, map[string]string{gate: "1"}, "")
			require.Error(t, err, output)
			require.Contains(t, output, "redis-server")
			require.NotContains(t, output, "--- SKIP:")
		})
	}
	t.Run("enabled-requires-non-C-UTF8", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "redis-server"), []byte("#!/bin/sh\nexit 99\n"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "locale"), []byte("#!/bin/sh\nprintf 'C\\nC.utf8\\n'\n"), 0700))
		output, err := runStandaloneGateHelper(t, map[string]string{"RELAIS_TEST_REDIS_REQUIRE": "1"}, dir)
		require.Error(t, err, output)
		require.Contains(t, output, "en_US UTF-8 locale required")
		require.NotContains(t, output, "--- SKIP:")
	})
}

func TestReleaseWorkflowSkipsOwnedStandaloneRedis(t *testing.T) {
	type step struct {
		Run string            `yaml:"run"`
		Env map[string]string `yaml:"env"`
	}
	var workflow struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Env   map[string]string `yaml:"env"`
			Steps []step            `yaml:"steps"`
		} `yaml:"jobs"`
	}
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, &workflow))
	checked := 0
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if !strings.Contains(step.Run, "make test-unit") {
				continue
			}
			checked++
			env := map[string]string{}
			for _, values := range []map[string]string{workflow.Env, job.Env, step.Env} {
				for _, key := range []string{"RELAIS_TEST_REDIS_REQUIRE", "RELAIS_TEST_REDIS_CLUSTER"} {
					if value, ok := values[key]; ok {
						env[key] = value
					}
				}
			}
			for _, value := range env {
				require.Contains(t, []string{"", "0"}, value, "release Redis gate must be known and disabled")
			}
			output, err := runStandaloneGateHelper(t, env, "")
			require.NoError(t, err, output)
			require.Contains(t, output, "--- SKIP:")
		}
	}
	require.Positive(t, checked, "release unit-test command must remain covered")
}
