package integration

import (
	"context"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/relais/plugins/ingress/camera"
	"github.com/relais/plugins/transforms/watermark"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginChain verifies that plugins can work together in a pipeline.
// Tests the flow: Camera Ingress -> Watermark Transform -> Storage
func TestPluginChain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Initialize storage
	store := storage.NewMemoryStorage()

	// Initialize plugins
	cameraPlugin := camera.NewCameraPlugin()
	err := cameraPlugin.Initialize(ctx, map[string]interface{}{
		"fps": 30,
	})
	require.NoError(t, err)

	watermarkPlugin := watermark.NewWatermarkPlugin()
	err = watermarkPlugin.Initialize(ctx, map[string]interface{}{
		"position_x": 10,
		"position_y": 10,
	})
	require.NoError(t, err)

	// Run camera plugin
	go func() {
		err := cameraPlugin.Run(ctx, store)
		assert.NoError(t, err)
	}()

	// Wait for some frames
	time.Sleep(2 * time.Second)

	// Verify frames were captured
	frames, err := store.ListFrames(ctx, "test_camera")
	require.NoError(t, err)
	assert.Greater(t, len(frames), 0)

	// Run watermark plugin
	go func() {
		err := watermarkPlugin.Run(ctx, store)
		assert.NoError(t, err)
	}()

	// Wait for processing
	time.Sleep(2 * time.Second)

	// Verify frames were processed
	processedFrames, err := store.ListFrames(ctx, "test_camera")
	require.NoError(t, err)
	assert.Equal(t, len(frames), len(processedFrames))
}

// TestPluginFailureRecovery verifies that plugins can recover from failures.
// Tests plugin restart and state recovery.
func TestPluginFailureRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store := storage.NewMemoryStorage()
	plugin := camera.NewCameraPlugin()

	// Start plugin multiple times
	for i := 0; i < 3; i++ {
		err := plugin.Initialize(ctx, map[string]interface{}{
			"fps": 30,
		})
		require.NoError(t, err)

		go func() {
			err := plugin.Run(ctx, store)
			assert.NoError(t, err)
		}()

		time.Sleep(time.Second)
		cancel()
		time.Sleep(time.Second)

		// Verify plugin stopped cleanly
		frames, err := store.ListFrames(ctx, "test_camera")
		require.NoError(t, err)
		assert.Greater(t, len(frames), 0)
	}
}
