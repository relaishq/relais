package callharness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Keep the actual Pion pair while a signaling client tries new credentials.
// This is the §7 case where the caller still uses the old pair after rollback.
func TestRelayRollbackTimeoutKeepsOldPairMovable(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		p, w, h, _, _ := relayChangeTopology(t, store, false, false)
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		c, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, err)
		c.eventsCancel()
		sent := make(chan error, 1)
		go func() { sent <- c.SendMedia(ctx, 23*time.Second) }()
		changed := make(chan error, 1)
		go func() { changed <- p.ChangeRelay(ctx, c.SessionID(), "r2") }()
		for generation := uint64(1); generation <= 2; generation++ {
			target := "r2"
			if generation == 2 {
				target = "r1"
			}
			require.Eventually(t, func() bool {
				_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
				return e == nil && a.Generation == generation-1 && a.Pending == target
			}, 12*time.Second, 10*time.Millisecond)
			lines := strings.Split(c.offer, "\r\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "a=ice-ufrag:") {
					lines[i] = fmt.Sprintf("a=ice-ufrag:pending%d", generation)
				}
				if strings.HasPrefix(line, "a=ice-pwd:") {
					lines[i] = "a=ice-pwd:pendingpasswordpendingpassword"
				}
			}
			req, e := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/ice-restart?generation=%d", c.resourceURL, generation), strings.NewReader(strings.Join(lines, "\r\n")))
			require.NoError(t, e)
			req.Header.Set("Content-Type", "application/sdp")
			resp, e := h.httpClient.Do(req)
			require.NoError(t, e)
			body, e := io.ReadAll(resp.Body)
			require.NoError(t, e)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 200, resp.StatusCode, string(body))
		}
		require.Error(t, <-changed)
		_, a, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}, a, "second timeout must clear pending state")
		before := c.rec.report()
		time.Sleep(500 * time.Millisecond)
		after := c.rec.report()
		for i, track := range after.Tracks {
			require.Greater(t, track.Packets, before.Tracks[i].Packets)
		}
		require.NoError(t, <-sent)
		failures, err := w.SessionDecryptFailures(c.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		state, err := w.ExportSession(c.SessionID())
		require.NoError(t, err, "rollback must revoke worker pending credentials so exact export works")
		require.NotEmpty(t, state)

	})
}
