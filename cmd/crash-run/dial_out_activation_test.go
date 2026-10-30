//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// Gate an actual worker-origin request before control sees it. Heartbeats and
// the other worker's command link use independent requests and remain live.
type activationBody struct {
	io.ReadCloser
	ctx     context.Context
	pending bytes.Buffer
	armed   *atomic.Bool
	entered chan struct{}
}

func (b *activationBody) Read(dst []byte) (int, error) {
	if b.pending.Len() == 0 {
		m, err := workerlink.Read(b.ReadCloser)
		if err != nil {
			return 0, err
		}
		if m.Kind == "request" && m.Path == "/leg-ready" && b.armed.CompareAndSwap(true, false) {
			close(b.entered)
			<-b.ctx.Done()
			return 0, b.ctx.Err()
		}
		if err := workerlink.Write(&b.pending, m); err != nil {
			return 0, err
		}
	}
	return b.pending.Read(dst)
}
func TestDialOutProcessDeathBeforeLegActivation(t *testing.T) {
	if os.Getenv("RELAIS_TEST_DIAL_OUT_PROCESS") != "1" {
		t.Skip("set RELAIS_TEST_DIAL_OUT_PROCESS=1 after make build")
	}
	for _, killed := range []string{"control", "worker"} {
		t.Run(killed, func(t *testing.T) {
			addr := redistestenv.Address(t)
			require.NotEmpty(t, addr)
			bin, err := filepath.Abs("../../bin")
			require.NoError(t, err)
			var key [32]byte
			_, err = rand.Read(key[:])
			require.NoError(t, err)
			env := processEnv(hex.EncodeToString(key[:]), addr, "dial-out-activation:"+rand.Text()+":")
			manager := &clusterprocess.Manager{}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			dir, err := os.MkdirTemp(bin, "dial-out-activation-")
			require.NoError(t, err)
			t.Logf("process logs: %s", dir)
			var proxy *httptest.Server
			var mapping *nettopology.NATMapping
			transport := workerlink.Transport()
			t.Cleanup(func() {
				manager.Stop(true)
				if proxy != nil {
					proxy.CloseClientConnections()
					proxy.Close()
				}
				transport.CloseIdleConnections()
				if mapping != nil {
					require.NoError(t, mapping.Close())
				}
				if !t.Failed() && os.Getenv("RELAIS_TEST_KEEP_PROCESS_LOGS") != "1" {
					_ = os.RemoveAll(dir)
				}
			})
			topology, err := newPairTopology(manager, dir, env, "", 0, 0)
			require.NoError(t, err)
			release := &pairRelease{Dir: bin, Version: "local"}
			require.NoError(t, topology.startRelay(ctx, release))
			startControl := func() *pairProc {
				p, err := topology.launch("control", "control", release, env, release.binary("relais-control"), "-http", topology.controlAddr, "-relay", "http://"+topology.relayHTTP, "-worker", "0=dial-out", "-worker", "1=dial-out")
				require.NoError(t, err)
				require.NoError(t, topology.ready(ctx, p, 5*time.Second))
				topology.control = p
				return p
			}
			control := startControl()
			mapping, err = nettopology.NewNATMapping(netip.MustParseAddrPort(topology.relayLeg))
			require.NoError(t, err)
			target, err := url.Parse("http://" + topology.controlAddr)
			require.NoError(t, err)
			reverse := httputil.NewSingleHostReverseProxy(target)
			reverse.Transport = transport
			reverse.FlushInterval = -1
			var armed atomic.Bool
			entered := make(chan struct{})
			proxy = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/private/workers/0/link" {
					r.Body = &activationBody{ReadCloser: r.Body, ctx: r.Context(), armed: &armed, entered: entered}
				}
				reverse.ServeHTTP(w, r)
			}))
			protocols := new(http.Protocols)
			protocols.SetHTTP1(true)
			protocols.SetUnencryptedHTTP2(true)
			proxy.Config.Protocols = protocols
			proxy.Start()
			startWorker := func(i int) *pairProc {
				leg, controlURL := topology.relayLeg, "http://"+topology.controlAddr
				if i == 0 {
					leg = mapping.WorkerAddr().String()
					controlURL = proxy.URL
				}
				p, err := topology.launch(pairWorkerName(i), "worker", release, env, release.binary("relais-worker"), "-connect=dial-out", "-name", strconv.Itoa(i), "-media", topology.workerMedia[i], "-relay-leg", leg, "-relay-media", topology.relayMedia, "-control", controlURL)
				require.NoError(t, err)
				topology.workers[i] = p
				require.NoError(t, topology.ready(ctx, p, 8*time.Second))
				return p
			}
			startWorker(0)
			startWorker(1)
			_, err = topology.waitControl(ctx, 8*time.Second, "dial-out readiness", func(s controlplane.Status) bool { return s.Ready && len(s.Workers) == 2 })
			require.NoError(t, err)
			h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: "http://" + topology.controlAddr + "/calls", RelayAddr: netip.MustParseAddrPort(topology.relayMedia)}})
			require.NoError(t, err)
			defer func() { _ = h.Close() }()
			c, err := h.Dial(ctx, callharness.CallOptions{Video: true, Worker: 0})
			require.NoError(t, err)
			media, stop := context.WithCancel(ctx)
			sent := make(chan error, 1)
			go func() { sent <- c.SendMedia(media, 40*time.Second) }()
			defer func() { stop(); <-sent }()
			time.Sleep(600 * time.Millisecond)
			armed.Store(true)
			began := time.Now()
			require.NoError(t, mapping.Rebind(netip.AddrPort{}))
			next := mapping.ObservedAddr()
			select {
			case <-entered:
			case <-time.After(6 * time.Second):
				t.Fatal("worker never reached pending leg activation")
			}
			status, err := topology.controlStatus(ctx)
			require.NoError(t, err)
			require.Equal(t, next, pairWorker(status, "0").Address)
			require.True(t, pairWorker(status, "0").Draining)
			require.Len(t, status.Calls, 1)
			require.Equal(t, "1", status.Calls[0].Owner)
			if killed == "control" {
				require.NoError(t, control.Child.SignalGroup(syscall.SIGKILL))
				<-control.Child.Done()
				startControl()
			} else {
				source := topology.workers[0]
				require.NoError(t, source.Child.SignalGroup(syscall.SIGKILL))
				<-source.Child.Done()
				_, err = topology.waitControl(ctx, 5*time.Second, "empty dead registration settled", func(s controlplane.Status) bool { w := pairWorker(s, "0"); return w != nil && w.Dead && !w.Recovering })
				require.NoError(t, err)
				startWorker(0)
			}
			status, err = topology.waitControl(ctx, 8*time.Second, "activation after process death", func(s controlplane.Status) bool {
				w := pairWorker(s, "0")
				return s.Ready && w != nil && !w.Dead && !w.Draining && w.Address == next && w.Link != nil && w.Link.Connected
			})
			if err != nil {
				t.Logf("activation status: %+v", status)
				for _, w := range status.Workers {
					t.Logf("worker: %+v", w)
					if w.Link != nil {
						t.Logf("link: %+v", *w.Link)
					}
				}
			}
			require.NoError(t, err)
			require.Empty(t, status.Takeovers)
			require.Zero(t, status.LostCount)
			relay := &controlplane.RemoteRelay{URL: "http://" + topology.relayHTTP}
			relayStatus, err := relay.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, 2, relayStatus.Stats.Workers, "activation/replacement must remove the old relay registry entry")
			require.NoError(t, c.MoveExternal(ctx, 0))
			time.Sleep(700 * time.Millisecond)
			report, err := c.Hangup(ctx)
			require.NoError(t, err)
			require.True(t, report.ConnectedThroughout())
			require.Zero(t, report.DecryptionFailures.Total())
			require.Zero(t, report.ICERestarts)
			require.Zero(t, report.Renegotiations)
			for i := range 2 {
				values, available, err := scrapeMetrics(ctx, &http.Client{Timeout: 2 * time.Second}, "http://"+topology.controlAddr+"/private/workers/"+strconv.Itoa(i)+"/metrics")
				require.NoError(t, err)
				require.True(t, available["relais_worker_decryption_failures_total"])
				require.Zero(t, values["relais_worker_decryption_failures_total"])
			}
			t.Logf("NAT_ACTIVATION_DEATH killed=%s recovery=%s false_takeovers=0 caller_decryption_failures=0 observed_worker_decryption_failures=0 relay_workers=2 logs=%s", killed, time.Since(began), dir)
		})
	}
}
