package config

import (
	"github.com/pion/webrtc/v3"
	"github.com/spf13/viper"
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
	viper.SetDefault("logging.level", "info")
	viper.SetDefault("webrtc.ice_servers", []string{"stun:stun.l.google.com:19302"})

	viper.AutomaticEnv()
	viper.SetEnvPrefix("RELAIS")

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

	return &config, nil
}
