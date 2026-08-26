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

// writeFileAtomic writes data to path via write-then-rename, so an interrupted
// write can never truncate the file it replaces. Every file rollout persists
// holds credentials or safety state, so the temp file is chmod'ed before it is
// written and the rename publishes it already at its final permissions.
//
// A symlinked path is resolved first. Renaming over a symlink replaces the link
// itself with a plain file, silently detaching a config that a dotfile manager
// owns and leaving the real target stale.
//
// Errors are returned unwrapped; callers name the file they were writing.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	path = resolveWritePath(path)
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// resolveWritePath returns the path writeFileAtomic will really write to,
// following symlinks to their target. A path that is not a link — including one
// that does not exist yet — is returned unchanged.
//
// Anything that checks whether a write will succeed has to ask about this path
// rather than the one it was handed: a symlink pointing into an unwritable
// directory looks perfectly writable until the rename lands there.
//
// A link whose target does not exist yet is followed too, which EvalSymlinks
// alone will not do. Deployment and dotfile tools lay down exactly that — a
// link staged before the file it points at — and renaming over it would replace
// the link with a plain file and leave the real target empty forever.
func resolveWritePath(path string) string {
	// Bounded so a symlink loop terminates instead of spinning.
	for range 16 {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		target, err := os.Readlink(path)
		if err != nil {
			return path // not a symlink, or unreadable: write here
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return path
}

func userConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, configDirName), nil
}
