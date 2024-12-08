// Package frames provides types and utilities for handling media frames.
package frames

// CodecType represents supported media codecs.
// These are the codecs that can be used for encoding/decoding frames.
type CodecType string

const (
	CodecH264 CodecType = "h264" // H.264/AVC video codec
	CodecVP8  CodecType = "vp8"  // VP8 video codec
	CodecVP9  CodecType = "vp9"  // VP9 video codec
	CodecOpus CodecType = "opus" // Opus audio codec
	CodecAAC  CodecType = "aac"  // AAC audio codec
)

// CodecParams contains codec-specific configuration.
// Used to configure encoders and decoders.
type CodecParams struct {
	Type      CodecType // Codec identifier
	Profile   string    // Codec profile (e.g., "baseline", "main", "high")
	Level     string    // Codec level (e.g., "3.1", "4.0")
	BitRate   int       // Target bitrate in bits per second
	FrameRate int       // Target frame rate for video
}

// IsVideo returns true if the codec is a video codec.
func (c CodecType) IsVideo() bool {
	return c == CodecH264 || c == CodecVP8 || c == CodecVP9
}

// IsAudio returns true if the codec is an audio codec.
func (c CodecType) IsAudio() bool {
	return c == CodecOpus || c == CodecAAC
}
