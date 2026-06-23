// Package store provides persistent state storage for the bot.
package store

import (
	"os"
	"path/filepath"
)

// DefaultStateDir returns ~/.wechatbot.
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".wechatbot"
	}
	return filepath.Join(home, ".wechatbot")
}

// ensureDir creates dir with 0700 permissions if it does not exist.
func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0700)
}
