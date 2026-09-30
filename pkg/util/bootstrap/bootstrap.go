package bootstrap

import (
	"context"

	"github.com/relais/pkg/buildinfo"
	"github.com/relais/pkg/config"
	"github.com/relais/pkg/logging"
	"github.com/relais/pkg/storage"
)

// Init sets up config, logger, and storage for runner binaries and logs startup info.
// process is the binary name (e.g., ingress-runner), pluginType is optional.
func Init(ctx context.Context, process string, pluginType string) (*logging.Logger, storage.Storage, *config.Config, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		// Fall back to default logger to emit fatal via caller
		return logging.NewLogger("info"), nil, nil, err
	}
	logger := logging.NewLogger(cfg.Logging.Level)

	// Build storage from config
	var store storage.Storage
	if cfg.Storage.Type == "redis" {
		// Prefer cluster config when enabled
		if cfg.Storage.RedisClusterEnable {
			rc := storage.RedisConfig{
				Addrs:    cfg.Storage.RedisClusterAddrs,
				Cluster:  true,
				Password: cfg.Storage.RedisPassword,
				DB:       cfg.Storage.RedisDB,
				Prefix:   cfg.Storage.RedisPrefix,
			}
			store, err = storage.NewRedisStorage(rc)
		} else {
			// Single-node via URL
			rc := storage.RedisConfig{
				Addr:     cfg.Storage.RedisURL,
				Cluster:  false,
				Password: cfg.Storage.RedisPassword,
				DB:       cfg.Storage.RedisDB,
				Prefix:   cfg.Storage.RedisPrefix,
			}
			// NewRedisStorage accepts either string or RedisConfig; use config for consistency
			store, err = storage.NewRedisStorage(rc)
		}
	} else {
		store = storage.NewMemoryStorage()
	}
	if err != nil {
		return logger, nil, cfg, err
	}

	// Emit startup structured info (include build info if available)
	fields := map[string]interface{}{
		"process":         process,
		"plugin_type":     pluginType,
		"storage.type":    cfg.Storage.Type,
		"storage.cluster": cfg.Storage.RedisClusterEnable,
		"log.level":       cfg.Logging.Level,
	}
	for k, v := range buildinfo.Fields() {
		fields[k] = v
	}
	logger.WithFields(fields).Info("startup")

	return logger, store, cfg, nil
}
