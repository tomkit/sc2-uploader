//go:build !darwin && !windows

package main

import (
	"os"
	"path/filepath"
)

// StarCraft II has no native Linux client. Under Wine/Lutris the folder
// depends on the prefix, so pass it with --dir.
func accountsRoots() []string { return nil }

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "sc2-uploader")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "sc2-uploader")
}
