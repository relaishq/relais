package harness

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"github.com/stretchr/testify/require"
)

// This uses an existing interface address, never adds an alias, and exercises
// the real launcher/Redis/relay/workers/control/signal/room on private binds.
func TestEveryServiceOnExistingPrivateAddress(t *testing.T) {
	bin := os.Getenv("RELAIS_NVWA_TEST_BIN")
	if bin == "" {
		t.Skip("set RELAIS_NVWA_TEST_BIN to built spike binaries")
	}
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	var private netip.Addr
	for _, raw := range addrs {
		prefix, e := netip.ParsePrefix(raw.String())
		if e == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() {
			private = prefix.Addr()
			break
		}
	}
	if !private.IsValid() {
		t.Skip("no existing RFC1918 interface address; validator tests still run")
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.Clean(filepath.Join(wd, "../../.."))
	a, err := media.LoadOpus(filepath.Join(filepath.Dir(bin), "tone-440.ogg"))
	require.NoError(t, err)
	b, err := media.LoadOpus(filepath.Join(filepath.Dir(bin), "tone-880.ogg"))
	require.NoError(t, err)
	tones := [][][]byte{a, b}
	for _, variant := range []string{"ws", "dc-reliable", "dc-unreliable"} {
		t.Run(variant, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			top, e := startBound(ctx, bin, root, t.TempDir(), variant != "ws", nil, true, private)
			require.NoError(t, e)
			defer top.Close()
			for _, raw := range []string{top.Config.RoomURL, top.Config.SignalURL, top.Config.Control, top.Config.Redis, top.Config.Media, top.Config.Audio} {
				endpoint := raw
				if index := strings.Index(endpoint, "://"); index >= 0 {
					endpoint = endpoint[index+3:]
				}
				parsed, e := netip.ParseAddrPort(endpoint)
				require.NoError(t, e)
				require.Equal(t, private, parsed.Addr())
			}
			sig := &media.Signal{URL: top.Config.SignalURL, CertFile: top.Config.Cert, TokenFile: top.Config.Token}
			require.NoError(t, sig.Init())
			var calls [2]*media.Call
			var consumers [2]*client.Client
			defer func() {
				for i := range 2 {
					if consumers[i] != nil {
						_ = consumers[i].Close()
					}
					if calls[i] != nil {
						calls[i].Close()
					}
				}
			}()
			for i, worker := range []string{"w1", "w2"} {
				creds, e := client.Lobby(ctx, top.Config.RoomURL)
				require.NoError(t, e)
				calls[i], e = media.DialPolicy(ctx, sig, worker, tones[i], tones[1-i], variant != "ws", variant == "dc-reliable")
				require.NoError(t, e)
				var conn client.MessageConn
				if variant == "ws" {
					conn, e = client.DialWS(client.WSURL(top.Config.RoomURL))
					require.NoError(t, e)
				} else {
					conn = calls[i].DataConn()
				}
				consumers[i], e = client.Connect(ctx, conn, creds, calls[i].ID)
				require.NoError(t, e)
			}
			require.Eventually(t, func() bool { return calls[0].Report().PeerPayload >= 10 && calls[1].Report().PeerPayload >= 10 }, 5*time.Second, 20*time.Millisecond, "two-way audio must work with private room bus")
			payload := json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
			reply, e := consumers[0].Request(ctx, room.Message{Type: "trigger", CommandID: "private-probe", InputSeq: 1, Payload: payload, Digest: room.Digest(payload)})
			require.NoError(t, e)
			require.Empty(t, reply.Error)
			require.NotNil(t, reply.Result)
			require.True(t, reply.Result.Applied)
			_, e = consumers[0].SendState(room.Message{Type: "state", Seq: 1, SentNS: time.Now().UnixNano(), Payload: json.RawMessage(`{"x":7}`)})
			require.NoError(t, e)
			require.Eventually(t, func() bool {
				for _, observation := range consumers[1].Observations() {
					if observation.Reply.Type == "state-update" && observation.Reply.Seq == 1 {
						return true
					}
				}
				return false
			}, 3*time.Second, 10*time.Millisecond)
			for _, call := range calls {
				report := call.Report()
				require.Zero(t, report.SelfPayload)
				require.Zero(t, report.UnknownPayload)
			}
		})
	}
}
