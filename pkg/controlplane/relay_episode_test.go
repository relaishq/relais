package controlplane

import (
	"bufio"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayEpisodeDeadlineSurvivesBusyRetriesAndReload(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		detected := time.Now()
		first, err := p.relayRecoveryEpisode(context.Background(), c, detected)
		require.NoError(t, err)
		defer first.once.Do(func() { close(first.done) })
		c.mu.Lock()
		again, err := p.relayRecoveryEpisode(context.Background(), c, detected.Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, first.deadline, again.deadline)
		c.mu.Unlock()
		// Reload from the same store into a fresh call object, as reconstruction does.
		restored := &call{id: id, lease: c.lease, relayCapable: true}
		saved, err := p.relayRecoveryEpisode(context.Background(), restored, detected.Add(time.Hour))
		require.NoError(t, err)
		defer saved.once.Do(func() { close(saved.done) })
		require.WithinDuration(t, detected.Add(15*time.Second), saved.deadline, time.Millisecond)
		require.Equal(t, first.intent.Token, saved.intent.Token)
	})
}

func TestRelayEpisodeExhaustionPublishesEndedWhileBusy(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		response, err := server.Client().Get(server.URL + "/calls/" + id + "/events")
		require.NoError(t, err)
		defer response.Body.Close()
		event := make(chan string, 1)
		go func() {
			scan := bufio.NewScanner(response.Body)
			ended := false
			for scan.Scan() {
				line := scan.Text()
				if line == "event: ended" {
					ended = true
				}
				if ended && strings.HasPrefix(line, "data: ") {
					event <- line
					return
				}
			}
			event <- ""
		}()
		func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			episode, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-14900*time.Millisecond))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				c.stateMu.Lock()
				defer c.stateMu.Unlock()
				return c.relayEndReason == "relay_change_failed"
			}, time.Second, time.Millisecond)
			require.Less(t, time.Until(episode.deadline), time.Duration(0))
			select {
			case data := <-event:
				require.Contains(t, data, "relay_change_failed")
			case <-time.After(time.Second):
				t.Fatal("busy operation prevented ended event")
			}
		}()
		require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }, time.Second, time.Millisecond)
	})
}

type episodeReadFault struct {
	*sessionstore.Memory
	fail atomic.Bool
}

func (s *episodeReadFault) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	if s.fail.Load() {
		return nil, errors.New("journal unavailable")
	}
	return s.Memory.ControlIntents(ctx)
}
func TestRelayEpisodeFailedJournalReadKeepsDetectionTime(t *testing.T) {
	store := &episodeReadFault{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	c, err := p.lookup(id)
	require.NoError(t, err)
	detected := time.Now()
	store.fail.Store(true)
	_, err = p.relayRecoveryEpisode(context.Background(), c, detected)
	require.Error(t, err)
	store.fail.Store(false)
	episode, err := p.relayRecoveryEpisode(context.Background(), c, detected.Add(time.Hour))
	require.NoError(t, err)
	defer episode.once.Do(func() { close(episode.done) })
	require.Equal(t, detected.Add(15*time.Second), episode.deadline)
}

func TestLostSlotPreservesRecoveryAlreadyTargetingLiveSlot(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
		_, assignment := waitRelayPending(t, store, id)
		require.Equal(t, "r2", assignment.Pending)
		var cancelled atomic.Bool
		c.controlMu.Lock()
		original := c.relayCancel
		c.relayCancel = func() { cancelled.Store(true); original() }
		c.controlMu.Unlock()
		defer func() {
			original()
			require.NoError(t, p.End(context.Background(), id))
			require.Eventually(t, func() bool {
				c.relayEpisodeMu.Lock()
				defer c.relayEpisodeMu.Unlock()
				return c.relayEpisode == nil || c.relayEpisode.completed.Load()
			}, time.Second, time.Millisecond)
		}()
		require.NoError(t, p.loseRelay(context.Background(), "r1"))
		require.False(t, cancelled.Load(), "loss and caller triggers must join the same live-target attempt")
	})
}
