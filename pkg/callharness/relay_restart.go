package callharness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/iceufrag"
	"github.com/relaishq/relais/internal/relaytrace"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/pkg/controlplane"
)

func (c *Call) startRelayEvents() {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		cancel()
		return
	}
	c.eventsCancel = cancel
	c.readers.Add(1)
	c.mu.Unlock()
	c.startReader(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if !c.relayStalled() {
					continue
				}
				c.relayRestartMu.Lock()
				waiting := time.Now().Before(c.relayAttemptUntil)
				c.relayRestartMu.Unlock()
				if waiting {
					continue
				}
				err := c.restartRelay(ctx, controlplane.RelayEvent{Deadline: time.Now().Add(15 * time.Second)})
				c.mu.Lock()
				c.restartErr = err
				c.mu.Unlock()
			}
		}
	})
	go func() {
		defer c.readers.Done()
		client := *c.harness.httpClient
		client.Timeout = 0
		var last uint64
		backoff := 50 * time.Millisecond
		for ctx.Err() == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resourceURL+"/events", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					scan := bufio.NewScanner(resp.Body)
					for scan.Scan() {
						line := scan.Text()
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event controlplane.RelayEvent
						if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							break
						}
						if event.Reason != "" {
							_ = c.pc.Close()
							cancel()
							_ = resp.Body.Close()
							return
						}
						if event.Generation <= last {
							continue
						}

						err = c.restartRelay(ctx, event)
						if ctx.Err() != nil {
							_ = resp.Body.Close()
							return
						}
						c.mu.Lock()
						c.restartErr = err // a higher-generation rollback can repair a refusal
						c.mu.Unlock()

						last = event.Generation
						backoff = 50 * time.Millisecond
					}
				}
				_ = resp.Body.Close()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(2*backoff, time.Second)
			}
		}
	}()
}

func (c *Call) restartRelay(ctx context.Context, event controlplane.RelayEvent) error {
	c.relayRestartMu.Lock()
	defer c.relayRestartMu.Unlock()
	if event.Generation != 0 && event.Generation <= c.relayGeneration {
		return nil
	}
	if event.Generation == 0 && (time.Now().Before(c.relayAttemptUntil) || !c.relayStalled()) {
		return nil
	}
	c.relayAttemptUntil = event.Deadline
	if event.Generation == 0 {
		c.relayAttemptUntil = time.Now().Add(2 * time.Second)
	}
	ctx, cancel := context.WithDeadline(ctx, event.Deadline)
	defer cancel()
	if relaytrace.Enabled() {
		relaytrace.Emit("offer-start-pair", c.selectedRemoteAddr(), nil)
	}

	// A refused offer remains outstanding in Pion. Reuse it for the recovery
	// answer; a caller with an applied answer starts a normal fresh restart.
	if c.pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		// CreateOffer starts gathering on an ICE restart. Subscribe first:
		// Pion can capture the previous GatheringCompletePromise handler before
		// a new promise is installed, then complete without waking that promise.
		// The handler stays installed until the next restart replaces it, so
		// close the channel at most once.
		gathered := make(chan struct{})
		gatherComplete := sync.OnceFunc(func() { close(gathered) })
		c.pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
			if state == webrtc.ICEGatheringStateComplete {
				gatherComplete()
			}
		})
		offer, err := c.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
		if err != nil {
			return err
		}
		if relaytrace.Enabled() {
			relaytrace.Emit("offer-created-pair", c.selectedRemoteAddr(), nil)
		}
		if err = c.pc.SetLocalDescription(offer); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gathered:
		}
	}

	endpoint := c.resourceURL + "/ice-restart"
	if event.Generation != 0 {
		endpoint += "?generation=" + strconv.FormatUint(event.Generation, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(c.pc.LocalDescription().SDP))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/sdp")
	relaytrace.Emit("offer-post", c.resourceURL, nil)
	resp, err := c.harness.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ICE restart refused: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	c.rec.offerAnswerExchange()
	err = c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(data)})
	if err == nil {
		_, generation, _ := iceufrag.Parse(iceUfrag(c.pc.RemoteDescription()))
		c.relayGeneration = generation
		// The event stream carries the actual server attempt bound. Spontaneous
		// requests have no event; permit another stall observation after two seconds.
		if event.Generation == 0 {
			c.relayAttemptUntil = time.Now().Add(2 * time.Second)
		}
	}
	if relaytrace.Enabled() {
		relaytrace.Emit("answer-applied", c.selectedRemoteAddr(), nil)
	}
	return err
}

func (c *Call) relayStalled() bool {
	last := c.inboundMedia.Load()
	return c.iceStalled.Load() || c.liveMedia.Load() && last != nil && time.Since(*last) >= 2*time.Second
}
