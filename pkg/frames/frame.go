package frames

import (
	"time"
)

// Frame represents a media frame with metadata
type Frame struct {
	SessionID string
	Index     int64
	Data      []byte
	Timestamp time.Time
	MediaType string // "video" or "audio"
	Codec     string
	KeyFrame  bool
}

// FrameMetadata contains frame information without the actual data
type FrameMetadata struct {
	SessionID string
	Index     int64
	Timestamp time.Time
	MediaType string
	Codec     string
	KeyFrame  bool
	Size      int
}

// NewFrame creates a new frame with the given parameters
func NewFrame(sessionID string, index int64, data []byte, mediaType, codec string) Frame {
	return Frame{
		SessionID: sessionID,
		Index:     index,
		Data:      data,
		Timestamp: time.Now(),
		MediaType: mediaType,
		Codec:     codec,
		KeyFrame:  false,
	}
}
