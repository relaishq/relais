package mediaworker

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/logging"
	"github.com/pion/sctp"
	"github.com/stretchr/testify/require"
)

// Both SCTP wire modes use real associations and the production transport /
// watchdog. The raw connection stands in for already-established component
// DTLS; its continued usability proves local SCTP shutdown does not own it.
func TestDataReassemblyWatchdogBothWireModes(t *testing.T) {
	for _, interleaving := range []bool{false, true} {
		t.Run(fmt.Sprint(interleaving), func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			transport := newDataTransport(left)
			go transport.pump()
			defer transport.Close()
			serverResult := make(chan *sctp.Association, 1)
			serverError := make(chan error, 1)
			go func() {
				a, err := sctp.ServerWithOptions(sctp.Config{NetConn: transport, MaxReceiveBufferSize: dataReceiveBytes, MaxMessageSize: 1 << 20})
				if err != nil {
					serverError <- err
					return
				}
				serverResult <- a
			}()
			client, err := sctp.ClientWithOptions(sctp.WithNetConn(right), sctp.WithMaxMessageSize(1<<20), sctp.WithEnableInterleaving(interleaving))
			require.NoError(t, err)
			defer client.Close()
			var server *sctp.Association
			select {
			case server = <-serverResult:
			case err := <-serverError:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("SCTP handshake timed out")
			}
			defer server.Close()
			metadata, ok := client.Metadata()
			require.True(t, ok)
			require.Equal(t, interleaving, metadata.MessageInterleavingEnabled)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := &dataSession{s: &session{ctx: ctx, worker: &Worker{}, log: logging.NewDefaultLoggerFactory().NewLogger("test")}, maxMessage: 1024, channels: make(map[string]*dataPipe), transport: transport}
			go d.watchReassembly(transport)
			factory := logging.NewDefaultLoggerFactory()
			clientDC, err := datachannel.Dial(client, 0, &datachannel.Config{Label: "control", ChannelType: datachannel.ChannelTypeReliable, LoggerFactory: factory})
			require.NoError(t, err)
			serverDC, err := datachannel.Accept(server, &datachannel.Config{LoggerFactory: factory})
			require.NoError(t, err)
			pipe := &dataPipe{owner: d, dc: serverDC, reliable: true, wake: make(chan struct{}, 1)}
			d.mu.Lock()
			d.channels["control"] = pipe
			d.mu.Unlock()
			go pipe.read()
			peerClosed := make(chan struct{})
			go func() { defer close(peerClosed); _, _, _ = clientDC.ReadDataChannel(make([]byte, dataReceiveBytes)) }()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err = clientDC.WriteDataChannel(make([]byte, 128<<10), false)
			require.NoError(t, err)
			pendingMax := 0
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			timeout := time.NewTimer(4 * time.Second)
			defer timeout.Stop()
		waiting:
			for {
				select {
				case <-transport.stopped:
					break waiting
				case <-tick.C:
					pendingMax = max(pendingMax, transport.monitor.pendingBytes())
				case <-timeout.C:
					t.Fatal("stalled fragments did not stop SCTP")
				}
			}
			require.EqualValues(t, 1, d.s.worker.DataChannelStats().Oversized)
			require.EqualValues(t, 1, d.s.worker.DataChannelStats().ReassemblyStalls)
			select {
			case <-peerClosed:
			case <-time.After(time.Second):
				t.Fatal("peer did not observe the channel reset")
			}

			// The pump must still wait for DTLS/application records, not have exited.
			select {
			case <-transport.readDone:
				t.Fatal("data shutdown closed the shared transport")
			default:
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			t.Logf("wire_mode_interleaving=%t pending_max=%d total_heap_delta=%d allocated_delta=%d (whole test process, not an RSS cap)", interleaving, pendingMax, int64(after.HeapAlloc)-int64(before.HeapAlloc), after.TotalAlloc-before.TotalAlloc)
		})
	}
}

func TestDataReassemblyWatchdogNewBacklogAfterIdle(t *testing.T) {
	packet := func(tsn uint32, flags byte, n int) []byte {
		raw := make([]byte, 12+16+n)
		raw[13] = flags
		binary.BigEndian.PutUint16(raw[14:16], uint16(16+n))
		binary.BigEndian.PutUint32(raw[16:20], tsn)
		binary.BigEndian.PutUint32(raw[24:28], 53) // binary user payload
		return raw
	}
	var monitor dataReadMonitor
	monitor.observe(packet(1, 3, 4), time.Now())
	monitor.consumed(0, 4)
	// Inject the arrival time at the protocol observer seam; no wall-clock sleep.
	resumed := time.Now().Add(10 * time.Second)
	monitor.observe(packet(2, 2, 2048), resumed)
	require.False(t, monitor.stalled(resumed.Add(500*time.Millisecond), 1024), "idle time before new input is not a reassembly stall")
	require.True(t, monitor.stalled(resumed.Add(dataReassemblyTimeout), 1024))
}
