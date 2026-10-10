package processrun

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessConfigurationRequiresDedicatedStore(t *testing.T) {
	for _, addr := range []string{"", "localhost:6379", "127.0.0.1:6379", "[::1]:6379", "not-an-address", "127.0.0.1:06379", "127.0.0.1:redis", "127.0.0.1:0"} {
		require.Error(t, ValidateRedis(addr), addr)
	}
	require.NoError(t, ValidateRedis("127.0.0.1:16379"))
	t.Setenv("RELAIS_SESSIONSTORE_KEY", "")
	_, err := (StoreConfig{Address: "127.0.0.1:1"}).Open(context.Background())
	require.ErrorContains(t, err, "32 bytes")
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	_, err = (StoreConfig{Address: "localhost:6379"}).Open(context.Background())
	require.ErrorContains(t, err, "reserved")
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", "192.168.1.1:0", ":0"} {
		_, err := ListenPrivate(addr)
		require.Error(t, err)
	}
	listener, err := ListenPrivate("127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestSecondSignalExitsDuringDrain(t *testing.T) {
	if os.Getenv("RELAIS_TEST_SIGNAL_CHILD") == "1" {
		ctx, stop := Context()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("draining")
		select {}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondSignalExitsDuringDrain$")
	cmd.Env = append(os.Environ(), "RELAIS_TEST_SIGNAL_CHILD=1")
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	lines := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	expect := func(line string) {
		t.Helper()
		select {
		case got := <-lines:
			require.Equal(t, line, got)
		case <-time.After(3 * time.Second):
			t.Fatalf("did not receive %s", line)
		}
	}
	expect("ready")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	expect("draining")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		status, ok := exit.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		require.Equal(t, syscall.SIGTERM, status.Signal())
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("second SIGTERM was ignored while draining")
	}
}
