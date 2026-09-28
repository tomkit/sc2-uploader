package main

import (
	"os"
	"path/filepath"
)

func accountsRoots() []string {
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, "Library", "Application Support", "Blizzard", "StarCraft II", "Accounts")}
}

func defaultStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "sc2-uploader")
}
