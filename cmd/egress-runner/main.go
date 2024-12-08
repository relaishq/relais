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
	"github.com/relais/plugins/egress/webrtc_egress"
)

func main() {
	pluginType := flag.String("type", "webrtc", "Type of egress plugin to run")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize logger
	logger := logging.NewLogger(cfg.Logging.Level)

	// Initialize storage
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

	// Initialize plugin
	var plugin plugins.EgressPlugin
	switch *pluginType {
	case "webrtc":
		plugin = webrtc_egress.NewWebRTCEgressPlugin()
	default:
		logger.Fatalf("Unknown plugin type: %s", *pluginType)
	}

	// Handle shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		cancel()
	}()

	// Run plugin
	if err := plugin.Run(ctx, store); err != nil {
		logger.Fatalf("Plugin error: %v", err)
	}
}
