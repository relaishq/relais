package callharness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/mediaworker"
)

// Kill simulates process death without a flush, export, release or close_notify.
// Recovery is driven solely by missed heartbeats, never by this action.
func (h *Harness) Kill(worker int) error {
	if h.workers.relay == nil || worker < 0 || worker >= len(h.workers.list) {
		return errors.New("callharness: kill needs a relayed worker index")
	}
	h.callsMu.Lock()
	name := strconv.Itoa(worker)
	h.failures[name] = append(h.failures[name], time.Now())
	h.callsMu.Unlock()
	return workerprobe.Kill(h.workers.list[worker].LocalAddr())
}

// RestartWorker creates an empty replacement on a new private socket after
// all the previous registration's leases have been handled.
func (h *Harness) RestartWorker(worker int) error {
	t := h.workers.relay
	if t == nil || worker < 0 || worker >= len(h.workers.list) {
		return errors.New("callharness: no relayed worker")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	old := h.workers.list[worker]
	fresh, err := mediaworker.New(mediaworker.Config{LoggerFactory: t.loggerFactory, SnapshotInterval: t.snapshotInterval,
		Relay: &mediaworker.RelayConfig{Addr: t.relay.WorkerAddr(), PublicAddr: t.relay.PublicAddr(), Owners: t.owners}})
	if err != nil {
		return err
	}
	t.relay.AddWorker(fresh.LocalAddr())
	if err := t.plane.Replace(strconv.Itoa(worker), fresh.LocalAddr(), fresh); err != nil {
		t.relay.RemoveWorker(fresh.LocalAddr())
		_ = fresh.Close()
		return err
	}
	t.relay.RemoveWorker(old.LocalAddr())
	h.workers.list[worker] = fresh
	return nil
}

func (c *Call) collectTakeovers(ctx context.Context) error {
	if c.harness.workers.relay == nil {
		return nil
	}
	status, err := c.harness.Status(ctx)
	if err != nil {
		return err
	}
	for _, res := range status.Takeovers {
		if res.ID != c.SessionID() {
			continue
		}
		from, _ := strconv.Atoi(res.From)
		to, _ := strconv.Atoi(res.To)
		start := res.LastHeartbeat
		c.harness.callsMu.Lock()
		actions := append([]time.Time{}, c.harness.failures[res.From]...)
		c.harness.callsMu.Unlock()
		for _, action := range actions {
			if !action.After(res.DetectedAt) {
				start = action
			}
		}
		var moveErr error
		if res.Error != "" {
			moveErr = errors.New(res.Error)
		}
		c.rec.move(moveRecord{kind: "takeover", detection: res.DetectedAt.Sub(start), from: from, to: to, start: start, end: res.End, result: res.Result, err: moveErr})
	}
	return nil
}

// KillBeforeSnapshot gates the next periodic copy and kills without flushing,
// so a stale-snapshot kill is deterministic rather than chosen by chance.
func (h *Harness) KillBeforeSnapshot(ctx context.Context, worker int, id string) error {
	if worker < 0 || worker >= len(h.workers.list) {
		return errors.New("callharness: no worker")
	}
	addr := h.workers.list[worker].LocalAddr()
	ready := make(chan struct{})
	var once sync.Once
	err := workerprobe.SetBeforeSnapshot(addr, func(lifetime context.Context, sessionID string) {
		if sessionID != id {
			return
		}
		once.Do(func() { close(ready) })
		select {
		case <-lifetime.Done():
		case <-ctx.Done():
		}
	})
	if err != nil {
		return err
	}
	defer func() { _ = workerprobe.SetBeforeSnapshot(addr, nil) }()
	select {
	case <-ready:
		return h.Kill(worker)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// KillMidKeyframe stops after n packets were echoed, before the marker packet.
// The cancellable gate prevents the worker from finishing the frame first.
func (h *Harness) KillMidKeyframe(ctx context.Context, worker int, id string, n int) error {
	if worker < 0 || worker >= len(h.workers.list) || n < 1 {
		return errors.New("callharness: invalid keyframe kill")
	}
	addr := h.workers.list[worker].LocalAddr()
	ready := make(chan struct{})
	var once sync.Once
	var timestamp uint32
	count := 0
	keyframe := false
	err := workerprobe.SetAfterEcho(addr, func(lifetime context.Context, sessionID string, raw []byte) {
		if sessionID != id {
			return
		}
		var packet rtp.Packet
		if packet.Unmarshal(raw) != nil || packet.PayloadType != vp8PayloadType {
			return
		}
		var desc codecs.VP8Packet
		payload, err := desc.Unmarshal(packet.Payload)
		if err != nil {
			return
		}
		if desc.S == 1 && desc.PID == 0 {
			timestamp, count, keyframe = packet.Timestamp, 0, isVP8Keyframe(payload)
		}
		if keyframe && timestamp == packet.Timestamp {
			count++
		}
		if keyframe && count == n && !packet.Marker {
			once.Do(func() { close(ready) })
			select {
			case <-lifetime.Done():
			case <-ctx.Done():
			}
		}
	})
	if err != nil {
		return err
	}
	defer func() { _ = workerprobe.SetAfterEcho(addr, nil) }()
	select {
	case <-ready:
		return h.Kill(worker)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForSnapshot observes the initial store write before a harness starts
// media. It does not flush or inspect state; this is only a test timing seam.
func (h *Harness) WaitForSnapshot(ctx context.Context, worker int, id string) error {
	if h.workers.relay == nil || worker < 0 || worker >= len(h.workers.list) {
		return fmt.Errorf("callharness: no relayed worker %d", worker)
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	addr := h.workers.list[worker].LocalAddr()
	for {
		if workerprobe.SnapshotReady(addr, id) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
