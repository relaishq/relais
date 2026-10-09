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
	webrtcegress "github.com/relais/plugins/egress/webrtc"
)

func main() {
	pluginType := flag.String("type", "webrtc", "Type of egress plugin to run")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bootstrap config, logger, storage and emit startup log
	logger, store, _, err := bootstrap.Init(ctx, "egress-runner", *pluginType)
	if err != nil {
		logging.NewLogger("info").WithError(err).Fatal("bootstrap failed")
	}
	defer store.Close()

	// Initialize plugin
	var plugin plugins.EgressPlugin
	switch *pluginType {
	case "webrtc":
		// Placeholder egress: it only polls storage and sends no media. Live
		// WebRTC media runs in the media worker (pkg/mediaworker).
		plugin = &webrtcegress.Egress{}
	default:
		logger.WithField("plugin_type", *pluginType).Fatal("unknown plugin type")
	}
	if err := plugin.Initialize(ctx, nil); err != nil {
		logger.WithError(err).Fatal("plugin initialize failed")
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
