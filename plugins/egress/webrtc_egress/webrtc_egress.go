package webrtc_egress

import (
	"context"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/storage"
)

// WebRTCEgressPlugin implements EgressPlugin for WebRTC output
type WebRTCEgressPlugin struct {
	peerConnection *webrtc.PeerConnection
	videoTrack     *webrtc.TrackLocalStaticSample
}

// NewWebRTCEgressPlugin creates a new WebRTC egress plugin
func NewWebRTCEgressPlugin() plugins.EgressPlugin {
	return &WebRTCEgressPlugin{}
}

func (p *WebRTCEgressPlugin) Initialize(ctx context.Context, config map[string]interface{}) error {
	// Initialize WebRTC peer connection
	mediaEngine := webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(&mediaEngine))
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}

	// Create video track
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: "video/h264"},
		"video",
		"relais-stream",
	)
	if err != nil {
		return err
	}

	if _, err = peerConnection.AddTrack(videoTrack); err != nil {
		return err
	}

	p.peerConnection = peerConnection
	p.videoTrack = videoTrack
	return nil
}

func (p *WebRTCEgressPlugin) Run(ctx context.Context, store storage.Storage) error {
	ticker := time.NewTicker(time.Second / 30) // 30 FPS
	defer ticker.Stop()

	var lastFrameIndex int64 = -1

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			frames, err := store.ListFrames(ctx, "current_session")
			if err != nil {
				continue
			}

			// Find new frames
			for _, frame := range frames {
				if frame.Index > lastFrameIndex {
					if err := p.videoTrack.WriteSample(media.Sample{
						Data:     frame.Data,
						Duration: time.Second / 30,
					}); err != nil {
						return err
					}
					lastFrameIndex = frame.Index
				}
			}
		}
	}
}

func (p *WebRTCEgressPlugin) Stop() error {
	if p.peerConnection != nil {
		return p.peerConnection.Close()
	}
	return nil
}
