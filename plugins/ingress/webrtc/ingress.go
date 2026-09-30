package webrtc_ingress

import (
	"context"
	"time"

	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/storage"
)

// Ingress captures media from a WebRTC source and writes frames to storage.
// This is a scaffold implementing plugins.IngressPlugin with TODOs for media wiring.
type Ingress struct {
	cfg   map[string]interface{}
	stopC chan struct{}
}

var _ plugins.IngressPlugin = (*Ingress)(nil)

func (i *Ingress) Initialize(ctx context.Context, config map[string]interface{}) error {
	i.cfg = config
	i.stopC = make(chan struct{})
	return nil
}

func (i *Ingress) Run(ctx context.Context, store storage.Storage) error {
	// TODO: Wire to an actual WebRTC PeerConnection and TrackRemote sources.
	// For now, simulate a minimal loop that exits on ctx cancellation.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var idx int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.stopC:
			return nil
		case <-ticker.C:
			// Example placeholder frame write (no real media)
			_ = store.PutFrame(ctx, storage.Frame{
				SessionID: "demo-session",
				Index:     idx,
				Data:      []byte("demo"),
				Timestamp: time.Now(),
				MediaType: "video",
				Codec:     "h264",
				KeyFrame:  false,
			})
			idx++
		}
	}
}

func (i *Ingress) Stop() error {
	select {
	case <-i.stopC:
		return nil
	default:
		close(i.stopC)
		return nil
	}
}
