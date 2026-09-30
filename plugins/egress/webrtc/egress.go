package webrtc_egress

import (
	"context"
	"time"

	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/storage"
)

// Egress reads frames from storage and outputs them via WebRTC.
// This is a scaffold implementing plugins.EgressPlugin with TODOs for media piping.
type Egress struct {
	cfg   map[string]interface{}
	stopC chan struct{}
}

var _ plugins.EgressPlugin = (*Egress)(nil)

func (e *Egress) Initialize(ctx context.Context, config map[string]interface{}) error {
	e.cfg = config
	e.stopC = make(chan struct{})
	return nil
}

func (e *Egress) Run(ctx context.Context, store storage.Storage) error {
	// TODO: Wire to WebRTC TrackLocal and feed samples from storage.
	// For now, simulate a minimal loop that exits on ctx cancellation.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.stopC:
			return nil
		case <-ticker.C:
			// Placeholder: read session list (ignore errors)
			_, _ = store.ListSessions(ctx)
		}
	}
}

func (e *Egress) Stop() error {
	select {
	case <-e.stopC:
		return nil
	default:
		close(e.stopC)
		return nil
	}
}
