package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	var cfg Config
	applyDefaults(&cfg)

	if cfg.Gateway.ID != "iot-gateway" {
		t.Errorf("gateway.id = %q, want iot-gateway", cfg.Gateway.ID)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("http.addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.Storage.SqlitePath != "gateway.db" {
		t.Errorf("storage.sqlitePath = %q, want gateway.db", cfg.Storage.SqlitePath)
	}
	if cfg.Scheduler.PoolSize != 16 {
		t.Errorf("scheduler.poolSize = %d, want 16", cfg.Scheduler.PoolSize)
	}
	if cfg.Log.Level != defaultLogLevel {
		t.Errorf("log.level = %q, want %q", cfg.Log.Level, defaultLogLevel)
	}
	if cfg.Log.Format != defaultLogFormat {
		t.Errorf("log.format = %q, want %q", cfg.Log.Format, defaultLogFormat)
	}
}

func TestApplyDefaultsLogFile(t *testing.T) {
	var cfg Config
	cfg.Log.File.Path = "./logs/gateway.log"
	applyDefaults(&cfg)

	if cfg.Log.File.MaxSize != defaultMaxSizeMB {
		t.Errorf("log.file.maxSize = %d, want %d", cfg.Log.File.MaxSize, defaultMaxSizeMB)
	}
	if cfg.Log.File.MaxBackups != defaultMaxBackups {
		t.Errorf("log.file.maxBackups = %d, want %d", cfg.Log.File.MaxBackups, defaultMaxBackups)
	}
	if cfg.Log.File.MaxAge != defaultMaxAgeDays {
		t.Errorf("log.file.maxAge = %d, want %d", cfg.Log.File.MaxAge, defaultMaxAgeDays)
	}
}

func TestLoadLogConfig(t *testing.T) {
	content := `
log:
  level: debug
  format: json
  file:
    path: "/var/log/gw.log"
    maxSize: 50
    maxBackups: 3
    maxAge: 14
    compress: true
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("level = %q, want debug", cfg.Log.Level)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("format = %q, want json", cfg.Log.Format)
	}
	if cfg.Log.File.Path != "/var/log/gw.log" {
		t.Errorf("file.path = %q, want /var/log/gw.log", cfg.Log.File.Path)
	}
	if cfg.Log.File.MaxSize != 50 {
		t.Errorf("maxSize = %d, want 50", cfg.Log.File.MaxSize)
	}
	if !cfg.Log.File.Compress {
		t.Error("compress = false, want true")
	}
}
