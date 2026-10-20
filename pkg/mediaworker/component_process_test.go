//go:build !windows

package mediaworker_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type splitWorkerProcess struct {
	cmd      *exec.Cmd
	done     chan error
	messages chan map[string]json.RawMessage
	addr     netip.AddrPort
	remote   *controlplane.RemoteWorker
}

func TestSplitCheckpointProcessSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("real SIGKILL and store takeover")
	}
	t.Run("standalone", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("dedicated Redis required")
			}
			t.Skip("dedicated Redis required")
		}
		testSplitProcessTakeover(t, []string{addr}, false, false)
	})
	t.Run("cluster", func(t *testing.T) {
		addrs := redistestcluster.Addresses(t)
		testSplitProcessTakeover(t, addrs, true, false)
	})
}
func TestSplitCheckpointProcessPlannedMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("separate worker processes and planned moves")
	}
	t.Run("standalone", func(t *testing.T) { testSplitProcessTakeover(t, []string{redistestcluster.Standalone(t)}, false, true) })
	t.Run("cluster", func(t *testing.T) { testSplitProcessTakeover(t, redistestcluster.Addresses(t), true, true) })
}

func testSplitProcessTakeover(t *testing.T, addrs []string, cluster, planned bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cfg := storage.RedisConfig{Addr: addrs[0], Prefix: fmt.Sprintf("split-process:%d:", time.Now().UnixNano())}
	if cluster {
		cfg.Addr = ""
		cfg.Addrs = addrs
		cfg.Cluster = true
	}
	store, err := sessionstore.NewRedis(ctx, cfg, make([]byte, 32))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	router, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, router.Close()) }()
	plane := controlplane.New(router, store)
	control := httptest.NewServer(plane.ProcessHandler())
	defer control.Close()
	start := func(name string, admit bool, working, history string) *splitWorkerProcess {
		p := &splitWorkerProcess{done: make(chan error, 1), messages: make(chan map[string]json.RawMessage, 32)}
		p.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSplitCheckpointWorkerProcess$")
		p.cmd.Env = append(os.Environ(), "RELAIS_SPLIT_PROCESS_HELPER=1", "RELAIS_SPLIT_PROCESS_REDIS="+strings.Join(addrs, ","), "RELAIS_SPLIT_PROCESS_PREFIX="+cfg.Prefix, "RELAIS_SPLIT_PROCESS_LEG="+router.WorkerAddr().String(), "RELAIS_SPLIT_PROCESS_PUBLIC="+router.PublicAddr().String(), "RELAIS_SPLIT_PROCESS_CONTROL="+control.URL, "RELAIS_SPLIT_PROCESS_NAME="+name, "RELAIS_SPLIT_PROCESS_ADMIT="+map[bool]string{true: "1", false: "0"}[admit], "RELAIS_SPLIT_PROCESS_WORKING="+working, "RELAIS_SPLIT_PROCESS_HISTORY="+history)
		output, err := p.cmd.StdoutPipe()
		require.NoError(t, err)
		p.cmd.Stderr = os.Stderr
		require.NoError(t, p.cmd.Start())
		go func() {
			scanner := bufio.NewScanner(output)
			for scanner.Scan() {
				var value map[string]json.RawMessage
				if json.Unmarshal(scanner.Bytes(), &value) == nil {
					p.messages <- value
				}
			}
			close(p.messages)
		}()
		go func() { p.done <- p.cmd.Wait() }()
		t.Cleanup(func() {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("owned worker did not exit")
			}
		})
		for {
			select {
			case value, ok := <-p.messages:
				require.True(t, ok, "worker exited before readiness")
				if value["http"] == nil {
					continue
				}
				var url, addr string
				require.NoError(t, json.Unmarshal(value["http"], &url))
				require.NoError(t, json.Unmarshal(value["media"], &addr))
				p.addr = netip.MustParseAddrPort(addr)
				p.remote = &controlplane.RemoteWorker{URL: url}
				router.AddWorker(p.addr)
				require.NoError(t, plane.Register(name, p.addr, p.remote))
				return p
			case <-ctx.Done():
				t.Fatal("worker readiness timeout")
				return nil
			}
		}
	}
	working, history := "working-state-byte-exact", "non-empty bounded test history: previous turn"
	source := start("0", true, working, history)
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = plane.Run(runCtx) }()
	defer func() { stop(); <-runDone }()
	harness, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: router.PublicAddr()}})
	require.NoError(t, err)
	defer harness.Close()
	call, err := harness.Dial(ctx, callharness.CallOptions{})
	require.NoError(t, err)
	var before sessionstore.ComponentTuple
	require.Eventually(t, func() bool {
		value, e := store.ReadComponents(ctx, call.SessionID())
		if e != nil {
			return false
		}
		var app, prior []byte
		if codec.Unmarshal(value.Components.Work, codec.Work, &app) != nil || codec.Unmarshal(value.Components.History, codec.History, &prior) != nil {
			return false
		}
		if !bytes.Equal(app, []byte(working)) || !bytes.Equal(prior, []byte(history)) {
			return false
		}
		before = value
		return true
	}, 6*time.Second, 5*time.Millisecond)
	require.Equal(t, source.addr, before.Lease.Worker)
	target := start("1", false, "", "") // no history source; new-call split admission is OFF
	if planned {
		for _, to := range []string{"1", "0"} {
			result, err := plane.Move(ctx, call.SessionID(), to)
			require.NoError(t, err, "planned move: %+v", result)
			owner := target.addr
			if to == "0" {
				owner = source.addr
			}
			require.Eventually(t, func() bool {
				value, err := store.ReadComponents(ctx, call.SessionID())
				if err != nil || value.Metadata.Owner != owner.String() {
					return false
				}
				require.Equal(t, before.Components.Work, value.Components.Work)
				require.Equal(t, before.Components.History, value.Components.History)
				return true
			}, 6*time.Second, 5*time.Millisecond)
			require.NoError(t, call.SendMedia(ctx, 200*time.Millisecond))
		}
		moves, err := plane.Drain(ctx, "0")
		require.NoError(t, err)
		require.Len(t, moves, 1)
		require.Eventually(t, func() bool {
			value, e := store.ReadComponents(ctx, call.SessionID())
			if e != nil || value.Metadata.Owner != target.addr.String() {
				return false
			}
			require.Equal(t, before.Components.Work, value.Components.Work)
			require.Equal(t, before.Components.History, value.Components.History)
			return true
		}, 6*time.Second, 5*time.Millisecond)
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
		return
	}
	require.NoError(t, source.cmd.Process.Kill())
	// The actual Restore callback reports the bytes it was given. A subsequent
	// factory default or an observer-only store read cannot satisfy this witness.
	restored := false
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	for !restored {
		select {
		case msg, ok := <-target.messages:
			require.True(t, ok)
			if raw, ok := msg["restored_working"]; ok {
				var value []byte
				require.NoError(t, json.Unmarshal(raw, &value))
				require.Equal(t, []byte(working), value)
				require.NoError(t, json.Unmarshal(msg["restored_history"], &value))
				require.Equal(t, []byte(history), value)
				restored = true
			}
		case <-deadline.C:
			t.Fatal("takeover Restore callback not observed")
		}
	}
	var after sessionstore.ComponentTuple
	require.Eventually(t, func() bool {
		value, e := store.ReadComponents(ctx, call.SessionID())
		if e != nil || value.Metadata.Owner != target.addr.String() || value.Metadata.OwnerEpoch <= before.Metadata.OwnerEpoch {
			return false
		}
		after = value
		return true
	}, 6*time.Second, 5*time.Millisecond)
	require.Equal(t, before.Components.Work, after.Components.Work)
	require.Equal(t, before.Components.History, after.Components.History)
	var oldCounters, newCounters codec.TransportState
	require.NoError(t, codec.Unmarshal(before.Components.Transport, codec.Transport, &oldCounters))
	require.NoError(t, codec.Unmarshal(after.Components.Transport, codec.Transport, &newCounters))
	require.GreaterOrEqual(t, newCounters.DTLS.SequenceNumber, oldCounters.DTLS.SequenceNumber+64, "crash adoption retains the DTLS reservation")
	require.NoError(t, call.SendMedia(ctx, 250*time.Millisecond))
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
	// Wait itself stays in cleanup; verify SIGKILL through the process state
	// after Wait has completed, without consuming the cleanup's completion token.
	select {
	case result := <-source.done:
		require.Error(t, result)
		source.done <- result
	case <-ctx.Done():
		t.Fatal("killed worker was not reaped")
	}
	require.Equal(t, syscall.SIGKILL, source.cmd.ProcessState.Sys().(syscall.WaitStatus).Signal())
	t.Logf("SIGKILL worker pid=%d; %d working bytes and %d history bytes restored exactly; admission-off survivor stayed v10; DTLS next %d -> %d", source.cmd.Process.Pid, len(working), len(history), oldCounters.DTLS.SequenceNumber, newCounters.DTLS.SequenceNumber)

}
