package test

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/png"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	webrtcegress "github.com/relais/plugins/egress/webrtc"
	"github.com/relais/plugins/ingress/camera"
	"github.com/relais/plugins/transforms/watermark"
	"github.com/stretchr/testify/assert"
)

func TestBasicMediaFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create in-memory storage
	store := storage.NewMemoryStorage()
	defer store.Close()

	// Initialize camera plugin
	camPlugin := camera.NewCameraPlugin()
	err := camPlugin.Initialize(ctx, map[string]interface{}{
		"device_id": "test_camera",
		"fps":       30,
	})
	assert.NoError(t, err)

	// Run plugin in background
	errChan := make(chan error, 1)
	go func() {
		err := camPlugin.Run(ctx, store)
		errChan <- err
	}()

	// Wait for some frames to be captured
	time.Sleep(2 * time.Second)

	// Cancel context to stop plugin
	cancel()

	// Check for errors from plugin
	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Errorf("Plugin error: %v", err)
		}
	case <-time.After(time.Second):
		// Plugin should have stopped by now
	}

	// Verify frames were stored (use new context since original is cancelled)
	verifyCtx := context.Background()
	frames, err := store.ListFrames(verifyCtx, "test_camera")
	assert.NoError(t, err)
	assert.Greater(t, len(frames), 0)
}

func TestFullPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create in-memory storage
	store := storage.NewMemoryStorage()
	defer store.Close()

	// Initialize camera plugin
	camPlugin := camera.NewCameraPlugin()
	err := camPlugin.Initialize(ctx, map[string]interface{}{
		"device_id": "test_camera",
		"fps":       30,
	})
	assert.NoError(t, err)

	// Initialize watermark plugin with test image
	watermarkPlugin := watermark.NewWatermarkPlugin()
	testWatermark := createTestWatermark(t)
	err = watermarkPlugin.Initialize(ctx, map[string]interface{}{
		"watermark_image": testWatermark,
		"position_x":      10,
		"position_y":      10,
	})
	assert.NoError(t, err)

	// Initialize the egress plugin (a storage-polling placeholder)
	egressPlugin := &webrtcegress.Egress{}
	err = egressPlugin.Initialize(ctx, nil)
	assert.NoError(t, err)

	// Run plugins in background with error channels
	errChans := make([]chan error, 3)
	for i := range errChans {
		errChans[i] = make(chan error, 1)
	}

	go func() {
		err := camPlugin.Run(ctx, store)
		errChans[0] <- err
	}()

	go func() {
		err := watermarkPlugin.Run(ctx, store)
		errChans[1] <- err
	}()

	go func() {
		err := egressPlugin.Run(ctx, store)
		errChans[2] <- err
	}()

	// Wait for some frames to be processed
	time.Sleep(3 * time.Second)

	// Stop plugins
	cancel()

	// Wait for all plugins to stop and check for errors
	for i, errChan := range errChans {
		select {
		case err := <-errChan:
			if err != nil && err != context.Canceled {
				t.Errorf("Plugin %d error: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("Plugin %d did not stop in time", i)
		}
	}

	// Verify frames were stored and processed (use new context)
	verifyCtx := context.Background()
	frames, err := store.ListFrames(verifyCtx, "test_camera")
	assert.NoError(t, err)
	assert.Greater(t, len(frames), 0)

	// Verify frame data exists (simplified test since we're using mock data)
	lastFrame := frames[len(frames)-1]
	assert.Greater(t, len(lastFrame.Data), 0)
	assert.Equal(t, "video", lastFrame.MediaType)
	assert.Equal(t, "test_camera", lastFrame.SessionID)
}

func createTestWatermark(t *testing.T) []byte {
	// Create a simple test watermark image
	img := image.NewRGBA(image.Rect(0, 0, 100, 30))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)

	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	assert.NoError(t, err)

	return buf.Bytes()
}
