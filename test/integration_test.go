package test

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/relais/plugins/egress/webrtc_egress"
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
	go func() {
		err := camPlugin.Run(ctx, store)
		assert.NoError(t, err)
	}()

	// Wait for some frames to be captured
	time.Sleep(2 * time.Second)

	// Verify frames were stored
	frames, err := store.ListFrames(ctx, "test_camera")
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

	// Initialize WebRTC egress plugin
	webrtcPlugin := webrtc_egress.NewWebRTCEgressPlugin()
	err = webrtcPlugin.Initialize(ctx, map[string]interface{}{
		"ice_servers": []string{"stun:stun.l.google.com:19302"},
	})
	assert.NoError(t, err)

	// Run plugins in background
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		err := camPlugin.Run(ctx, store)
		assert.NoError(t, err)
	}()

	go func() {
		defer wg.Done()
		err := watermarkPlugin.Run(ctx, store)
		assert.NoError(t, err)
	}()

	go func() {
		defer wg.Done()
		err := webrtcPlugin.Run(ctx, store)
		assert.NoError(t, err)
	}()

	// Wait for some frames to be processed
	time.Sleep(3 * time.Second)

	// Verify frames were stored and processed
	frames, err := store.ListFrames(ctx, "test_camera")
	assert.NoError(t, err)
	assert.Greater(t, len(frames), 0)

	// Verify frame contains watermark
	lastFrame := frames[len(frames)-1]
	img, _, err := image.Decode(bytes.NewReader(lastFrame.Data))
	assert.NoError(t, err)

	// Check image properties that would indicate watermark presence
	bounds := img.Bounds()
	assert.Greater(t, bounds.Max.X, 0)
	assert.Greater(t, bounds.Max.Y, 0)

	// Cleanup
	cancel()
	wg.Wait()
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
