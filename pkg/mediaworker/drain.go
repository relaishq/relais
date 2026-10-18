package mediaworker

import (
	"context"
	"sync"
)

// taskDrain fences launches across all actors. It never holds a session lock
// while waiting, and does not depend on snapshots or playback progress.
type taskDrain struct {
	mu        sync.Mutex
	quiescing bool
	active    int
	idle      chan struct{}
}

func (d *taskDrain) finish() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active--
	if d.active == 0 {
		close(d.idle)
	}
}

// BeginDrain refuses raced creates after control acknowledges the placement
// mark. Existing sessions and the private API remain live.
func (w *Worker) BeginDrain() {
	w.mu.Lock()
	w.draining = true
	w.mu.Unlock()
}

// QuiesceTasks stops new v2 task launches and waits concurrently for existing
// handlers and their terminal mailbox admissions, except ReadOnly tasks whose
// kinds opt out via Registration.SkipDrainWait. Starts remain never_launched.
// Actors, pacers, checkpoints and heartbeats continue independently. The launch
// fence remains set after the context ends, through the following urgent drain.
func (w *Worker) QuiesceTasks(ctx context.Context) error {
	d := &w.taskDrain
	d.mu.Lock()
	d.quiescing = true
	active, idle := d.active, d.idle
	d.mu.Unlock()
	if active == 0 {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
