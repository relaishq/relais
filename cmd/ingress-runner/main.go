// Package main implements the ingress plugin runner.
// It loads and executes plugins that capture media from external sources.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/relais/pkg/config"
	"github.com/relais/pkg/logging"
	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/storage"
	"github.com/relais/plugins/ingress/camera"
)

func main() {
	// Parse command-line flags for plugin selection
	pluginType := flag.String("type", "camera", "Type of ingress plugin to run")
	flag.Parse()

	// Setup context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize logger
	logger := logging.NewLogger(cfg.Logging.Level)

	// Initialize storage backend
	var store storage.Storage
	if cfg.Storage.Type == "redis" {
		store, err = storage.NewRedisStorage(cfg.Storage.RedisURL)
	} else {
		store = storage.NewMemoryStorage()
	}
	if err != nil {
		logger.Fatalf("Failed to initialize storage: %v", err)
	}
	defer store.Close()

	// Initialize the selected plugin
	var plugin plugins.IngressPlugin
	switch *pluginType {
	case "camera":
		plugin = camera.NewCameraPlugin()
	default:
		logger.Fatalf("Unknown plugin type: %s", *pluginType)
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		cancel()
	}()

	// Run the plugin
	if err := plugin.Run(ctx, store); err != nil {
		logger.Fatalf("Plugin error: %v", err)
	}
}
