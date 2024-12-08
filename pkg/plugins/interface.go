// Package plugins defines the core plugin interfaces and types for Relais.
// The plugin system is designed to be extensible, allowing different types of media
// processing components to be added without modifying the core system.
package plugins

import (
	"context"

	"github.com/relais/pkg/storage"
)

// Plugin represents the base interface for all plugin types.
// All plugins must implement these basic lifecycle methods to ensure proper
// initialization and cleanup of resources. This interface serves as the foundation
// for more specific plugin types like ingress, egress, and transform plugins.
type Plugin interface {
	// Initialize sets up the plugin with configuration.
	// This is called before Run() to prepare the plugin's resources.
	//
	// Parameters:
	//   - ctx: Context for initialization timeout and cancellation
	//   - config: Map of configuration parameters specific to the plugin
	//
	// Returns an error if initialization fails.
	Initialize(ctx context.Context, config map[string]interface{}) error

	// Stop gracefully shuts down the plugin and cleans up resources.
	// This should handle cleanup of any allocated resources such as:
	//   - Closing network connections
	//   - Releasing hardware resources
	//   - Saving state if necessary
	//   - Shutting down goroutines
	//
	// Returns an error if cleanup fails.
	Stop() error
}

// IngressPlugin defines the interface for media source plugins.
// These plugins capture media from external sources and write to storage.
// Examples of ingress plugins include:
//   - Camera capture
//   - RTSP stream ingestion
//   - File import
//   - Network stream reception
type IngressPlugin interface {
	Plugin
	// Run starts ingesting media and writing to storage.
	// It should continue running until context is cancelled.
	//
	// Parameters:
	//   - ctx: Context for lifecycle management and cancellation
	//   - store: Storage interface for writing captured frames
	//
	// The implementation should:
	//   1. Start capturing media from the source
	//   2. Convert media to appropriate frame format
	//   3. Write frames to storage with proper metadata
	//   4. Handle errors and retry logic appropriately
	//   5. Stop gracefully when context is cancelled
	Run(ctx context.Context, store storage.Storage) error
}

// EgressPlugin defines the interface for media output plugins.
// These plugins read from storage and deliver media to destinations.
// Examples of egress plugins include:
//   - WebRTC streaming
//   - File export
//   - RTMP streaming
//   - Network protocols (RTP, RTCP)
type EgressPlugin interface {
	Plugin
	// Run starts reading from storage and outputting media.
	// It should continue running until context is cancelled.
	//
	// Parameters:
	//   - ctx: Context for lifecycle management and cancellation
	//   - store: Storage interface for reading frames
	//
	// The implementation should:
	//   1. Monitor storage for new frames
	//   2. Process frames as needed (e.g., transcoding)
	//   3. Deliver frames to the destination
	//   4. Handle network conditions and buffering
	//   5. Implement appropriate error handling
	Run(ctx context.Context, store storage.Storage) error
}

// TransformPlugin defines the interface for media processing plugins.
// These plugins modify frames in the storage system.
// Examples of transform plugins include:
//   - Watermarking
//   - Video effects
//   - Frame rate conversion
//   - Resolution scaling
//   - Object detection/tracking
type TransformPlugin interface {
	Plugin
	// Run starts processing frames from storage.
	// It should read frames, transform them, and write back to storage.
	//
	// Parameters:
	//   - ctx: Context for lifecycle management and cancellation
	//   - store: Storage interface for reading and writing frames
	//
	// The implementation should:
	//   1. Monitor storage for new frames
	//   2. Apply transformations to frames
	//   3. Write modified frames back to storage
	//   4. Handle errors appropriately
	//   5. Optimize for performance and resource usage
	Run(ctx context.Context, store storage.Storage) error
}
