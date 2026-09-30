package config

import (
	"github.com/pion/webrtc/v3"
	"github.com/spf13/viper"
	"strings"
)

// Config holds all configuration for the application
type Config struct {
	Server  ServerConfig
	Storage StorageConfig
	Logging LoggingConfig
	WebRTC  WebRTCConfig
}

type ServerConfig struct {
	Host string
	Port int
}

type StorageConfig struct {
	Type     string // "redis" or "memory"
	RedisURL string
	// Redis Streams and retention options
	RedisStreamsEnable bool  `mapstructure:"redis_streams_enable"`
	RedisStreamsMaxLen int64 `mapstructure:"redis_streams_maxlen"`
	// Redis Streams consumer group options
	RedisStreamsGroupEnable bool   `mapstructure:"redis_streams_group_enable"`
	RedisStreamsGroup       string `mapstructure:"redis_streams_group"`
	RedisStreamsConsumer    string `mapstructure:"redis_streams_consumer"`
	RetentionSession        int    `mapstructure:"retention_session"`
	RetentionTrack          int    `mapstructure:"retention_track"`
	// Redis Cluster options
	RedisClusterEnable bool     `mapstructure:"redis_cluster_enable"`
	RedisClusterAddrs  []string `mapstructure:"redis_cluster_addrs"`
	RedisPassword      string   `mapstructure:"redis_password"`
	RedisDB            int      `mapstructure:"redis_db"`
	RedisPrefix        string   `mapstructure:"redis_prefix"`
}

type LoggingConfig struct {
	Level string
	File  string
}

type WebRTCConfig struct {
	ICEServers []webrtc.ICEServer
}

// LoadConfig reads configuration from environment variables and files
func LoadConfig() (*Config, error) {
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("storage.type", "memory")
	viper.SetDefault("storage.redis_url", "localhost:6379")
	// Streams/retention defaults
	viper.SetDefault("storage.redis_streams_enable", false)
	viper.SetDefault("storage.redis_streams_maxlen", 10000)
	viper.SetDefault("storage.redis_streams_group_enable", false)
	viper.SetDefault("storage.redis_streams_group", "relais")
	viper.SetDefault("storage.redis_streams_consumer", "")
	viper.SetDefault("storage.retention_session", 0)
	viper.SetDefault("storage.retention_track", 0)
	// Redis cluster defaults
	viper.SetDefault("storage.redis_cluster_enable", false)
	viper.SetDefault("storage.redis_cluster_addrs", []string{})
	viper.SetDefault("storage.redis_password", "")
	viper.SetDefault("storage.redis_db", 0)
	viper.SetDefault("storage.redis_prefix", "")
	viper.SetDefault("logging.level", "info")
	viper.SetDefault("webrtc.ice_servers", []string{"stun:stun.l.google.com:19302"})

	// Ensure environment variables override nested keys by mapping dots to underscores
	viper.SetEnvPrefix("RELAIS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, err
	}

	// Convert string ICE servers to proper ICEServer objects
	iceURLs := viper.GetStringSlice("webrtc.ice_servers")
	config.WebRTC.ICEServers = make([]webrtc.ICEServer, len(iceURLs))
	for i, url := range iceURLs {
		config.WebRTC.ICEServers[i] = webrtc.ICEServer{
			URLs: []string{url},
		}
	}

	// Basic validation and sane defaults
	if config.Storage.RedisStreamsEnable && config.Storage.RedisStreamsMaxLen <= 0 {
		// Default to 10000 when streams are enabled but maxlen not set
		config.Storage.RedisStreamsMaxLen = 10000
	}
	if config.Storage.RetentionSession < 0 {
		config.Storage.RetentionSession = 0
	}
	if config.Storage.RetentionTrack < 0 {
		config.Storage.RetentionTrack = 0
	}
	if config.Storage.RedisClusterEnable {
		// Require at least one address via cluster addrs or single redis_url
		if len(config.Storage.RedisClusterAddrs) == 0 && config.Storage.RedisURL == "" {
			// Fall back to default single-node address for convenience
			config.Storage.RedisURL = viper.GetString("storage.redis_url")
		}
	}

	return &config, nil
}
