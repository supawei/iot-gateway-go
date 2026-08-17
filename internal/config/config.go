package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	defaultLogLevel   = "info"
	defaultLogFormat  = "text"
	defaultMaxSizeMB  = 100
	defaultMaxBackups = 7
	defaultMaxAgeDays = 30
)

// Config 是网关自身配置,来自 YAML 文件。
type Config struct {
	Gateway struct {
		ID string `yaml:"id"`
	} `yaml:"gateway"`
	HTTP struct {
		Addr string `yaml:"addr"`
	} `yaml:"http"`
	Auth struct {
		Enabled    *bool  `yaml:"enabled"` // 默认 true;显式 false 关闭鉴权(逃生舱)
		SessionTTL string `yaml:"sessionTtl"`
	} `yaml:"auth"`
	Storage struct {
		SqlitePath string `yaml:"sqlitePath"`
	} `yaml:"storage"`
	Scheduler struct {
		PoolSize int `yaml:"poolSize"`
	} `yaml:"scheduler"`
	Log struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
		File   struct {
			Path       string `yaml:"path"`
			MaxSize    int    `yaml:"maxSize"`
			MaxBackups int    `yaml:"maxBackups"`
			MaxAge     int    `yaml:"maxAge"`
			Compress   bool   `yaml:"compress"`
		} `yaml:"file"`
	} `yaml:"log"`
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
	if cfg.Scheduler.PoolSize <= 0 {
		cfg.Scheduler.PoolSize = 16
	}
	if cfg.Auth.Enabled == nil {
		enabled := true
		cfg.Auth.Enabled = &enabled
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultLogLevel
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = defaultLogFormat
	}
	if cfg.Log.File.Path != "" {
		if cfg.Log.File.MaxSize == 0 {
			cfg.Log.File.MaxSize = defaultMaxSizeMB
		}
		if cfg.Log.File.MaxBackups == 0 {
			cfg.Log.File.MaxBackups = defaultMaxBackups
		}
		if cfg.Log.File.MaxAge == 0 {
			cfg.Log.File.MaxAge = defaultMaxAgeDays
		}
	}
}
