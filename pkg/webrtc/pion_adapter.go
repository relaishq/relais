package webrtc

import (
	"github.com/pion/webrtc/v3"
)

// WebRTCConfig holds configuration for WebRTC connections
type WebRTCConfig struct {
	ICEServers []webrtc.ICEServer
	MaxRetries int
}

// PionAdapter manages WebRTC connections using Pion
type PionAdapter struct {
	config WebRTCConfig
	api    *webrtc.API
}

// NewPionAdapter creates a new WebRTC adapter
func NewPionAdapter(config WebRTCConfig) (*PionAdapter, error) {
	mediaEngine := webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(&mediaEngine))

	return &PionAdapter{
		config: config,
		api:    api,
	}, nil
}

// CreatePeerConnection creates a new WebRTC peer connection
func (p *PionAdapter) CreatePeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: p.config.ICEServers,
	}

	return p.api.NewPeerConnection(config)
}
