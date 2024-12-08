package watermark

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/png"
	"time"

	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/storage"
)

// WatermarkPlugin implements TransformPlugin for adding watermarks
type WatermarkPlugin struct {
	watermark image.Image
	position  image.Point
}

// NewWatermarkPlugin creates a new watermark transform plugin
func NewWatermarkPlugin() plugins.TransformPlugin {
	return &WatermarkPlugin{}
}

func (p *WatermarkPlugin) Initialize(ctx context.Context, config map[string]interface{}) error {
	// Load watermark image from config
	if watermarkData, ok := config["watermark_image"].([]byte); ok {
		watermark, err := png.Decode(bytes.NewReader(watermarkData))
		if err != nil {
			return err
		}
		p.watermark = watermark
	}

	// Set watermark position
	if x, ok := config["position_x"].(int); ok {
		if y, ok := config["position_y"].(int); ok {
			p.position = image.Point{X: x, Y: y}
		}
	}

	return nil
}

func (p *WatermarkPlugin) Run(ctx context.Context, store storage.Storage) error {
	// Process frames in a loop
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Get list of sessions
			sessions, err := store.ListSessions(ctx)
			if err != nil {
				continue
			}

			// Process each session
			for _, sessionID := range sessions {
				frames, err := store.ListFrames(ctx, sessionID)
				if err != nil {
					continue
				}

				// Process each frame
				for _, frame := range frames {
					// Skip non-video frames
					if frame.MediaType != "video" {
						continue
					}

					// Decode image
					img, _, err := image.Decode(bytes.NewReader(frame.Data))
					if err != nil {
						continue
					}

					// Create output image
					bounds := img.Bounds()
					out := image.NewRGBA(bounds)
					draw.Draw(out, bounds, img, image.Point{}, draw.Src)

					// Apply watermark
					watermarkPos := p.position
					if watermarkPos.X < 0 {
						watermarkPos.X = bounds.Max.X - p.watermark.Bounds().Max.X + watermarkPos.X
					}
					if watermarkPos.Y < 0 {
						watermarkPos.Y = bounds.Max.Y - p.watermark.Bounds().Max.Y + watermarkPos.Y
					}
					draw.Draw(out, p.watermark.Bounds().Add(watermarkPos), p.watermark, image.Point{}, draw.Over)

					// Encode back to bytes
					var buf bytes.Buffer
					if err := png.Encode(&buf, out); err != nil {
						continue
					}

					// Update frame with watermarked data
					frame.Data = buf.Bytes()
					if err := store.PutFrame(ctx, frame); err != nil {
						continue
					}
				}
			}

			// Sleep briefly to avoid tight loop
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (p *WatermarkPlugin) Stop() error {
	return nil
}
