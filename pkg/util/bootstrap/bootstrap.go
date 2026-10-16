package bootstrap

import (
	"context"
	"os"

	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/buildinfo"
	"github.com/relaishq/relais/pkg/config"
	"github.com/relaishq/relais/pkg/logging"
	"github.com/relaishq/relais/pkg/storage"
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
		transport, tlsErr := redisendpoint.TLSFromEnv()
		if tlsErr != nil {
			return logger, nil, cfg, tlsErr
		}
		rc := storage.RedisConfig{
			Addr:      cfg.Storage.RedisURL,
			Cluster:   cfg.Storage.RedisClusterEnable,
			Username:  os.Getenv("RELAIS_REDIS_USERNAME"),
			Password:  cfg.Storage.RedisPassword,
			TLSConfig: transport,
			DB:        cfg.Storage.RedisDB,
			Prefix:    cfg.Storage.RedisPrefix,
		}
		if rc.Cluster {
			rc.Addrs = cfg.Storage.RedisClusterAddrs
		}
		store, err = storage.NewRedisStorage(rc)
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
