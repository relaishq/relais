package callharness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/controlplane"
)

// MoveExternal uses the operator HTTP endpoint and records the same caller
// yardstick as Handover. Worker names in the process driver are integer IDs.
// A definite control_not_ready refusal before any move starts can wait for
// readiness within the original two-second API budget. Started or uncertain
// outcomes are never replayed.
func (c *Call) MoveExternal(ctx context.Context, to int) error {
	if c.rec.distributed != nil {
		return errors.New("callharness: distributed moves require an armed identity and durable server receipt")
	}
	if c.harness.external == nil || to < 0 {
		return errors.New("callharness: external move needs an external topology and worker ID")
	}
	c.harness.statusPollMu.RLock()
	defer c.harness.statusPollMu.RUnlock()
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, privateapi.Timeout)
	defer cancel()
	var result controlplane.MoveResult
	var err, lastRefusal error
	for {
		if ctx.Err() != nil {
			err = errors.Join(lastRefusal, ctx.Err())
			break
		}
		result = controlplane.MoveResult{}
		err = c.harness.controlRequest(ctx, "POST", fmt.Sprintf("/calls/%s/move?to=%d", c.SessionID(), to), &result)
		if err == nil || !errors.Is(err, agentv2.ErrControlNotReady) || errors.Is(err, privateapi.ErrUncertain) || !result.Start.IsZero() {
			break
		}
		lastRefusal = err
		select {
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
		case <-time.After(50 * time.Millisecond):
			continue
		}
		break
	}
	if err != nil && ctx.Err() != nil {
		err = errors.Join(lastRefusal, err, ctx.Err())
	}
	if result.Start.IsZero() {
		result.Start, result.End = started, time.Now()
	}
	from, parseErr := strconv.Atoi(result.From)
	if parseErr != nil {
		from = -1
	}
	recordErr := err
	if result.Error != "" {
		recordErr = errors.New(result.Error)
	}
	c.rec.move(moveRecord{from: from, to: to, start: result.Start, end: result.End, result: result.Result, lost: result.Lost, lossCause: result.LossCause, err: recordErr})
	return err
}

// RecordExternalMove records a completed operator move (for example a drain).
// It observes the real control-plane result; it does not initiate recovery.
func (c *Call) RecordExternalMove(result controlplane.MoveResult) {
	from, err := strconv.Atoi(result.From)
	if err != nil {
		from = -1
	}
	to, err := strconv.Atoi(result.To)
	if err != nil {
		to = -1
	}
	var moveErr error
	if result.Error != "" {
		moveErr = errors.New(result.Error)
	}
	c.rec.move(moveRecord{from: from, to: to, start: result.Start, end: result.End, result: result.Result, lost: result.Lost, lossCause: result.LossCause, err: moveErr})
}
