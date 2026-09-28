package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// The Documents folder is often not %USERPROFILE%\Documents: OneDrive and
// folder redirection move it. Ask Windows for the real one first, then try
// the usual places; every existing candidate is watched.
func accountsRoots() []string {
	var docs []string
	if d, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && d != "" {
		docs = append(docs, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		docs = append(docs,
			filepath.Join(home, "Documents"),
			filepath.Join(home, "OneDrive", "Documents"),
		)
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range docs {
		root := filepath.Join(d, "StarCraft II", "Accounts")
		key := filepath.Clean(root)
		if !seen[key] {
			seen[key] = true
			out = append(out, root)
		}
	}
	return out
}

func defaultStateDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "sc2-uploader")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "AppData", "Local", "sc2-uploader")
}
