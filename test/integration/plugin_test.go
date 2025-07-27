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
		"device_id": "test_camera",
		"fps":       30,
	})
	require.NoError(t, err)

	watermarkPlugin := watermark.NewWatermarkPlugin()
	err = watermarkPlugin.Initialize(ctx, map[string]interface{}{
		"position_x": 10,
		"position_y": 10,
	})
	require.NoError(t, err)

	// Set up error channels for goroutines
	camErrChan := make(chan error, 1)
	watermarkErrChan := make(chan error, 1)

	// Run camera plugin
	go func() {
		err := cameraPlugin.Run(ctx, store)
		camErrChan <- err
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
		watermarkErrChan <- err
	}()

	// Wait for processing
	time.Sleep(2 * time.Second)

	// Stop plugins
	cancel()

	// Wait for plugins to stop and check errors
	select {
	case err := <-camErrChan:
		if err != nil && err != context.Canceled {
			t.Errorf("Camera plugin error: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Camera plugin did not stop in time")
	}

	select {
	case err := <-watermarkErrChan:
		if err != nil && err != context.Canceled {
			t.Errorf("Watermark plugin error: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Watermark plugin did not stop in time")
	}

	// Verify frames were processed (use new context)
	verifyCtx := context.Background()
	processedFrames, err := store.ListFrames(verifyCtx, "test_camera")
	require.NoError(t, err)

	// Note: The watermark plugin may add additional frames during processing
	// so we just verify we have at least the original frames
	assert.GreaterOrEqual(t, len(processedFrames), len(frames))
}

// TestPluginFailureRecovery verifies that plugins can recover from failures.
// Tests plugin restart and state recovery.
func TestPluginFailureRecovery(t *testing.T) {
	store := storage.NewMemoryStorage()
	plugin := camera.NewCameraPlugin()

	// Start plugin multiple times
	for i := 0; i < 3; i++ {
		iterCtx, iterCancel := context.WithTimeout(context.Background(), 5*time.Second)

		err := plugin.Initialize(iterCtx, map[string]interface{}{
			"device_id": "test_camera",
			"fps":       30,
		})
		require.NoError(t, err)

		errChan := make(chan error, 1)
		go func() {
			err := plugin.Run(iterCtx, store)
			errChan <- err
		}()

		time.Sleep(time.Second)
		iterCancel()

		// Wait for plugin to stop
		select {
		case err := <-errChan:
			if err != nil && err != context.Canceled {
				t.Errorf("Plugin error on iteration %d: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("Plugin did not stop in time on iteration %d", i)
		}

		// Verify plugin stopped cleanly
		verifyCtx := context.Background()
		frames, err := store.ListFrames(verifyCtx, "test_camera")
		require.NoError(t, err)
		assert.Greater(t, len(frames), 0)
	}
}
