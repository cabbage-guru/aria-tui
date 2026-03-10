package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

type Config struct {
	MaxConcurrent    int    `json:"max_concurrent"`
	DownloadDir      string `json:"download_dir"`
	StaleTimeoutMins int    `json:"stale_timeout_mins"`
}

func DefaultConfig() *Config {
	return &Config{
		MaxConcurrent:    5,
		DownloadDir:      filepath.Join(homeDir(), "Downloads", "aria-tui"),
		StaleTimeoutMins: 2,
	}
}

func ConfigDir() string {
	dir := filepath.Join(homeDir(), ".config", "aria-tui")
	os.MkdirAll(dir, 0700)
	return dir
}

func WireGuardDir() string {
	dir := filepath.Join(ConfigDir(), "wireguard")
	os.MkdirAll(dir, 0700)
	return dir
}

func SocketPath() string {
	return filepath.Join(ConfigDir(), "aria-tui.sock")
}

func HistoryPath() string {
	return filepath.Join(ConfigDir(), "history.json")
}

func QueuePath() string {
	return filepath.Join(ConfigDir(), "queue.json")
}

func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

func Load() (*Config, error) {
	path := ConfigPath()
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, cfg.Save()
		}
		return nil, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), data, 0600)
}

func homeDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("USERPROFILE")
	}
	home, _ := os.UserHomeDir()
	return home
}
