package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// StarCraft II writes every multiplayer replay (ladder, vs AI, custom and
// team games) to
//
//	<Accounts root>/<account id>/<region-realm-toon>/Replays/Multiplayer/*.SC2Replay
//
// The Accounts root is per-OS (accountsRoots, in paths_<os>.go). Campaign
// and Challenge replays live in sibling folders and are not uploaded.

const replayExt = ".sc2replay"

func isReplayFile(path string) bool {
	return strings.EqualFold(filepath.Ext(path), replayExt)
}

// multiplayerDirs finds every Replays/Multiplayer folder under an Accounts
// root. Cheap: two directory levels, no recursion into the replays.
func multiplayerDirs(root string) []string {
	var out []string
	accounts, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, a := range accounts {
		if !a.IsDir() {
			continue
		}
		toons, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			continue
		}
		for _, t := range toons {
			if !t.IsDir() {
				continue
			}
			dir := filepath.Join(root, a.Name(), t.Name(), "Replays", "Multiplayer")
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				out = append(out, dir)
			}
		}
	}
	return out
}

// watchRoots are the folders to watch: explicit --dir folders as given, or
// every Multiplayer folder under the OS's Accounts roots.
func watchRoots(explicit []string) (dirs []string, accountRoots []string) {
	if len(explicit) > 0 {
		for _, d := range explicit {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				dirs = append(dirs, d)
			}
		}
		return dirs, nil
	}
	for _, root := range accountsRoots() {
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			accountRoots = append(accountRoots, root)
			dirs = append(dirs, multiplayerDirs(root)...)
		}
	}
	return dirs, accountRoots
}

var toonRe = regexp.MustCompile(`^[0-9]+-S2-[0-9]+-[0-9]+$`)

// toons lists the StarCraft II toon ids on this computer: the per-region
// folder names under each account (`<Accounts>/<account>/1-S2-1-1240773`),
// which are the ids replays record for each player. With --dir folders,
// the toon is the folder two levels up from Replays/Multiplayer.
func toons(explicit []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if toonRe.MatchString(name) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(explicit) > 0 {
		for _, d := range explicit {
			add(filepath.Base(filepath.Dir(filepath.Dir(filepath.Clean(d)))))
		}
		return out
	}
	for _, root := range accountsRoots() {
		accounts, _ := os.ReadDir(root)
		for _, a := range accounts {
			if !a.IsDir() {
				continue
			}
			ts, _ := os.ReadDir(filepath.Join(root, a.Name()))
			for _, t := range ts {
				if t.IsDir() {
					add(t.Name())
				}
			}
		}
	}
	return out
}

// stateDir holds the config, the upload record, the log and the lock.
func stateDir() string {
	if d := os.Getenv("SC2_UPLOADER_HOME"); d != "" {
		return d
	}
	return defaultStateDir()
}
