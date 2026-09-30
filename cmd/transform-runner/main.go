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
	"github.com/relais/plugins/transforms/watermark"
)

func main() {
	pluginType := flag.String("type", "watermark", "Type of transform plugin to run")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bootstrap config, logger, storage and emit startup log
	logger, store, _, err := bootstrap.Init(ctx, "transform-runner", *pluginType)
	if err != nil {
		logging.NewLogger("info").WithError(err).Fatal("bootstrap failed")
	}
	defer store.Close()

	// Initialize plugin
	var plugin plugins.TransformPlugin
	switch *pluginType {
	case "watermark":
		plugin = watermark.NewWatermarkPlugin()
	default:
		logger.WithField("plugin_type", *pluginType).Fatal("unknown plugin type")
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
		logger.WithError(err).Fatal("plugin failed")
	}
}
