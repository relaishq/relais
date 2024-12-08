package benchmark

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"time"

	"github.com/relais/pkg/storage"
)

// VideoGenerator creates synthetic video frames for testing and benchmarking.
// It generates a moving pattern to simulate real video content.
type VideoGenerator struct {
	width     int           // Width of generated frames
	height    int           // Height of generated frames
	frameRate int           // Frames per second
	duration  time.Duration // Total duration of generated video
}

// NewVideoGenerator creates a new video generator with specified parameters.
// width and height define frame dimensions, frameRate sets FPS, and duration sets total video length.
func NewVideoGenerator(width, height, frameRate int, duration time.Duration) *VideoGenerator {
	return &VideoGenerator{
		width:     width,
		height:    height,
		frameRate: frameRate,
		duration:  duration,
	}
}

// GenerateFrames creates a sequence of test frames with a moving pattern.
// Returns a slice of Frame objects containing the generated video data.
func (g *VideoGenerator) GenerateFrames() []storage.Frame {
	frameCount := int(g.duration.Seconds() * float64(g.frameRate))
	frames := make([]storage.Frame, frameCount)

	for i := 0; i < frameCount; i++ {
		// Create a test pattern image
		img := image.NewRGBA(image.Rect(0, 0, g.width, g.height))

		// Draw a moving pattern - alternating red and blue stripes
		offset := i % g.width
		for y := 0; y < g.height; y++ {
			for x := 0; x < g.width; x++ {
				if (x+offset)%50 < 25 {
					img.Set(x, y, color.RGBA{255, 0, 0, 255}) // Red
				} else {
					img.Set(x, y, color.RGBA{0, 0, 255, 255}) // Blue
				}
			}
		}

		// Encode image to JPEG
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
			continue
		}

		// Create frame with metadata
		frames[i] = storage.Frame{
			SessionID:  "test_session",
			Index:     int64(i),
			Data:      buf.Bytes(),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second / time.Duration(g.frameRate)),
			MediaType: "video",
			Codec:     "jpeg",
			KeyFrame:  true,
		}
	}

	return frames
}
