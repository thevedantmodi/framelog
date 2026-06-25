package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// UserConfig is the persisted user-settable configuration. Written by the
// set_backup_path IPC command; read at daemon startup to override env-var defaults.
type UserConfig struct {
	BackupPath string `json:"backup_path"`
}

// ReadUserConfig loads UserConfig from UserConfigPath. Returns a zero-value
// UserConfig (not an error) when the file does not exist yet.
func ReadUserConfig() (UserConfig, error) {
	data, err := os.ReadFile(UserConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return UserConfig{}, nil
	}
	if err != nil {
		return UserConfig{}, err
	}
	var cfg UserConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, err
	}
	return cfg, nil
}

// WriteUserConfig atomically writes cfg to UserConfigPath.
func WriteUserConfig(cfg UserConfig) error {
	if err := os.MkdirAll(filepath.Dir(UserConfigPath), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := UserConfigPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, UserConfigPath)
}
