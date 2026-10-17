package loopback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Play waits outside the callback budget, then replays immutable encoded
// packets. Partitioning and sequence numbers depend only on the request. After
// skips already accepted chunks on a retried attempt, including an exact move.
func Play(ctx context.Context, run *agent.TaskRun) error {
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	request := run.Spec().Request
	recording, err := packets(request)
	if err != nil {
		return run.Fail(agent.Failed, err.Error())
	}
	sum := sha256.Sum256(request)
	checksum := []byte(hex.EncodeToString(sum[:]))
	var sequence uint64
	for start := 0; start < len(recording); {
		end := start
		var span time.Duration
		// Bounded work over an at-most-64 KiB immutable request, in a task handler.
		// Normal 20 ms input produces exactly five packets per 100 ms chunk. Other
		// durations preserve packet boundaries; the last chunk may be shorter.
		for end < len(recording) && span < 100*time.Millisecond {
			d, err := duration(recording[end])
			if err != nil {
				return run.Fail(agent.Failed, err.Error())
			}
			span += d
			end++
		}
		sequence++
		if sequence > run.After() {
			if err := run.Emit(ctx, sequence, agent.Chunk{Data: checksum, Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: recording[start:end]}}); err != nil {
				return err
			}
		}
		start = end
	}
	return nil
}
