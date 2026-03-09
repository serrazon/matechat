package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Broker string `json:"broker"`
	Cert   string `json:"cert"`
	Key    string `json:"key"`
	CA     string `json:"ca"`
}

func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".matechat"), nil
}

// Load reads ~/.matechat/config.json.
// Returns zero-value Config (no error) if file doesn't exist.
func Load() (Config, error) {
	dir, err := ConfigDir()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	// Resolve relative paths relative to ~/.matechat/
	cfg.Cert = resolvePath(dir, cfg.Cert)
	cfg.Key = resolvePath(dir, cfg.Key)
	cfg.CA = resolvePath(dir, cfg.CA)
	return cfg, nil
}

func resolvePath(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
