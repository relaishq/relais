package callharness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/relais/pkg/controlplane"
)

// MoveExternal uses the operator HTTP endpoint and records the same caller
// yardstick as Handover. Worker names in the process driver are integer IDs.
func (c *Call) MoveExternal(ctx context.Context, to int) error {
	if c.harness.external == nil || to < 0 {
		return errors.New("callharness: external move needs an external topology and worker ID")
	}
	started := time.Now()
	var result controlplane.MoveResult
	err := c.harness.controlRequest(ctx, "POST", fmt.Sprintf("/calls/%s/move?to=%d", c.SessionID(), to), &result)
	if result.Start.IsZero() {
		result.Start, result.End = started, time.Now()
	}
	from, _ := strconv.Atoi(result.From)
	c.rec.move(moveRecord{from: from, to: to, start: result.Start, end: result.End, result: result.Result, err: err})
	return err
}
