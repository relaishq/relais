package agentbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// A packet prepared before the cut can reach admission after the actor pauses.
// The host leaves that identity unconsumed. It must remain pending for the
// destination, rather than reaching the bot as a failed output packet.
func TestBridgeMovingReceiptClosesSocketWithoutPacketRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt mediaworker.BridgeOutputReceipt
		stop    bool
	}{
		{"moving", mediaworker.BridgeOutputReceipt{Outcome: "refused", Reason: "moving"}, true},
		{"late", mediaworker.BridgeOutputReceipt{Outcome: "refused", Reason: "output_late", Consumed: true}, false},
		{"pressure", mediaworker.BridgeOutputReceipt{Outcome: "refused", Reason: "backpressure"}, false},
		{"sent", mediaworker.BridgeOutputReceipt{Outcome: "sent", Consumed: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			replies := make(chan any, 1)
			replies <- typed("output_receipt", tc.receipt)
			stopped := make(chan error, 1)
			server := httptest.NewServer(websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(c *websocket.Conn) {
				defer func() { _ = c.Close() }()
				stopped <- a.bridge.write(ctx, c, a, mediaworker.BridgeOutputScope{}, replies, nil)
			}})
			defer server.Close()
			conn, err := dialBot(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/bridge")
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
			var message struct {
				Type     string `json:"type"`
				Outcome  string `json:"outcome"`
				Reason   string `json:"reason"`
				Consumed bool   `json:"consumed"`
			}
			err = websocket.JSON.Receive(conn, &message)
			if tc.stop {
				require.Error(t, err, "moving must close without publishing a packet refusal")
			} else {
				require.NoError(t, err)
				require.Equal(t, "output_receipt", message.Type)
				require.Equal(t, tc.receipt.Outcome, message.Outcome)
				require.Equal(t, tc.receipt.Reason, message.Reason)
				require.Equal(t, tc.receipt.Consumed, message.Consumed)
				cancel()
			}
			select {
			case err := <-stopped:
				if tc.stop {
					require.True(t, errors.Is(err, mediaworker.ErrBridgeOutputMoving))
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("socket writer did not stop")
			}
		})
	}
}
