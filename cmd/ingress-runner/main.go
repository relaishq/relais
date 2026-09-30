// Package main implements the ingress plugin runner.
// It loads and executes plugins that capture media from external sources.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/relais/pkg/logging"
	"github.com/relais/pkg/plugins"
	"github.com/relais/pkg/util/bootstrap"
	"github.com/relais/plugins/ingress/camera"
)

func main() {
	// Parse command-line flags for plugin selection
	pluginType := flag.String("type", "camera", "Type of ingress plugin to run")
	flag.Parse()

	// Setup context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bootstrap config, logger, storage and emit startup log
	logger, store, _, err := bootstrap.Init(ctx, "ingress-runner", *pluginType)
	if err != nil {
		logging.NewLogger("info").WithError(err).Fatal("bootstrap failed")
	}
	defer store.Close()

	// Initialize the selected plugin
	var plugin plugins.IngressPlugin
	switch *pluginType {
	case "camera":
		plugin = camera.NewCameraPlugin()
	default:
		logger.WithField("plugin_type", *pluginType).Fatal("unknown plugin type")
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
		logger.WithError(err).Fatal("plugin failed")
	}
}
