// Package camera implements an ingress plugin that simulates a camera input.
package camera

import (
	"context"
	"time"

	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/storage"
)

// CameraPlugin implements IngressPlugin for camera input.
// It generates simulated video frames at a specified frame rate.
type CameraPlugin struct {
	deviceID string // Unique identifier for the camera device
	fps      int    // Frames per second to generate
}

// NewCameraPlugin creates a new camera ingress plugin with default settings.
func NewCameraPlugin() plugins.IngressPlugin {
	return &CameraPlugin{
		fps: 30, // Default to 30 FPS
	}
}

// Initialize sets up the camera plugin with configuration parameters.
// Supported config options:
// - device_id: string - Unique identifier for the camera
// - fps: int - Frames per second to generate
func (p *CameraPlugin) Initialize(ctx context.Context, config map[string]interface{}) error {
	if deviceID, ok := config["device_id"].(string); ok {
		p.deviceID = deviceID
	}
	if fps, ok := config["fps"].(int); ok {
		p.fps = fps
	}
	return nil
}

// Run starts generating simulated video frames and storing them.
// Frames are generated at the configured FPS rate until context is cancelled.
func (p *CameraPlugin) Run(ctx context.Context, store storage.Storage) error {
	ticker := time.NewTicker(time.Second / time.Duration(p.fps))
	defer ticker.Stop()

	frameIndex := int64(0)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Create a simulated video frame
			frame := storage.Frame{
				SessionID: p.deviceID,
				Index:     frameIndex,
				Timestamp: time.Now(),
				MediaType: "video",
				Data:      []byte("mock frame data"), // In real implementation, this would be actual frame data
			}

			if err := store.PutFrame(ctx, frame); err != nil {
				return err
			}

			frameIndex++
		}
	}
}

// Stop cleans up any resources used by the camera plugin.
func (p *CameraPlugin) Stop() error {
	// Cleanup resources if needed
	return nil
}
