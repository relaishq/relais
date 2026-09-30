// Package main implements the core Relais media server.
// It handles session management, signaling, and plugin coordination.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/relais/pkg/buildinfo"
	"github.com/relais/pkg/logging"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/server"
	"github.com/relais/pkg/storage"
	"github.com/relais/pkg/util/bootstrap"
	"github.com/relais/pkg/webrtc"
)

func main() {
	// Parse command line flags
	// configFile := flag.String("config", "", "Path to configuration file")
	flag.Parse()

	// Setup context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Unified init: config, logger, storage, and startup log
	logger, store, cfg, err := bootstrap.Init(ctx, "relais-core", "")
	if err != nil {
		logging.NewLogger("info").WithError(err).Fatal("bootstrap init failed")
	}
	defer store.Close()

	// Configure Redis retention and streams from config
	if rs, ok := store.(*storage.RedisStorage); ok {
		if cfg.Storage.RetentionSession > 0 || cfg.Storage.RetentionTrack > 0 {
			rs.SetRetention(cfg.Storage.RetentionSession, cfg.Storage.RetentionTrack)
		}
		if cfg.Storage.RedisStreamsEnable {
			rs.EnableStreams(true, cfg.Storage.RedisStreamsMaxLen)
			// Optional: enable stream consumer groups
			if cfg.Storage.RedisStreamsGroupEnable {
				rs.EnableStreamGroups(true, cfg.Storage.RedisStreamsGroup, cfg.Storage.RedisStreamsConsumer)
			}
		}
	}

	// Initialize WebRTC adapter
	webrtcCfg := webrtc.WebRTCConfig{
		ICEServers: cfg.WebRTC.ICEServers,
		MaxRetries: 3,
	}
	webrtcAdapter, err := webrtc.NewPionAdapter(webrtcCfg)
	if err != nil {
		logger.Fatalf("Failed to initialize WebRTC: %v", err)
	}

	// Create session manager
	sessionMgr := server.NewSessionManager()

	// Initialize control plane and signaling handlers
	controlPlane := server.NewControlPlane(sessionMgr, store)
	signalingServer := server.NewSignalingServer(sessionMgr, webrtcAdapter)

	// Wire storage into signaling
	signalingServer.SetStorage(store)

	// Setup HTTP routing
	mux := http.NewServeMux()
	controlPlane.RegisterRoutes(mux)
	mux.HandleFunc("/ws/signaling", signalingServer.HandleWebSocket)

	// Health and build info endpoints
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctxReady, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()
		if _, err := store.ListSessions(ctxReady); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(buildinfo.Fields())
	})

	mux.Handle("/metrics", promhttp.Handler())

	// Start HTTP server
	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler: metrics.Middleware(mux),
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info("Shutting down gracefully...")
		if err := srv.Shutdown(ctx); err != nil {
			logger.Errorf("HTTP server shutdown error: %v", err)
		}
		cancel()
	}()

	// Start server
	logger.Infof("Relais core server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logger.Fatalf("HTTP server error: %v", err)
	}
}
