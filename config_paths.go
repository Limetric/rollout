package main

import (
	"os"
	"path/filepath"
)

// configDirName is the per-user subdirectory under the OS config dir
// (~/.config/rollout on Linux, ~/Library/Application Support/rollout on macOS).
// It holds config.toml plus state/ — the confirm-token store, the audit log,
// and tokens/ (the OAuth refresh-token store).
const configDirName = "rollout"

// defaultConfigFile is the file consulted when --config is not given.
const defaultConfigFile = "config.toml"

// resolveConfigPath returns the config file to read. An explicit path is used
// as-is. Otherwise the default path is returned only if it exists; a missing
// default file is not an error (env-only operation is supported, and is how
// MCP hosts and CI usually run).
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	dir, err := userConfigDir()
	if err != nil {
		return "", nil // no usable config dir → env only
	}
	p := filepath.Join(dir, defaultConfigFile)
	if _, err := os.Stat(p); err != nil {
		return "", nil
	}
	return p, nil
}

// stateDirPath is where the confirm-token store, the audit log, and the OAuth
// token store live. It only computes the path — nothing is created, so it is
// safe on read-only and diagnostic paths (`rollout doctor`, `rollout config show`).
func stateDirPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state"), nil
}

// stateDir is stateDirPath, created on demand. Callers should treat a returned
// error as "persistence disabled".
func stateDir() (string, error) {
	p, err := stateDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

func userConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, configDirName), nil
}
