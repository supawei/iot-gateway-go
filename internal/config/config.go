package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"iot-gateway-go/internal/output/mqtt"
)

// Config 是网关自身配置,来自 YAML 文件。
type Config struct {
	Gateway struct {
		ID string `yaml:"id"`
	} `yaml:"gateway"`
	HTTP struct {
		Addr string `yaml:"addr"`
	} `yaml:"http"`
	MQTT    mqtt.Config `yaml:"mqtt"`
	Storage struct {
		SqlitePath string `yaml:"sqlitePath"`
	} `yaml:"storage"`
}

func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Gateway.ID == "" {
		cfg.Gateway.ID = "iot-gateway"
	}
	if cfg.HTTP.Addr == "" {
		cfg.HTTP.Addr = ":8080"
	}
	if cfg.Storage.SqlitePath == "" {
		cfg.Storage.SqlitePath = "gateway.db"
	}
}
