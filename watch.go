package main

// dirWatcher reports paths that changed under the watched folders, using
// the OS's native change feed, so the uploader sleeps between games:
//
//   - macOS: FSEvents, one stream over the StarCraft II Accounts folder
//     (recursive, no per-file handles). watch_darwin.go.
//   - Windows: ReadDirectoryChangesW on each replay folder; Linux: inotify.
//     Both via fsnotify, which uses a handle per folder, not per file.
//     watch_fsnotify.go.
//
// Events are hints: the uploader re-checks the file (size settled, replay
// header, hash) before doing anything, and a slow rescan backs them up.
type dirWatcher interface {
	// Add starts watching a folder (a no-op when an existing watch covers it).
	Add(dir string) error
	// Paths delivers changed files and folders.
	Paths() <-chan string
	// Errors delivers watch failures; the uploader rescans on any.
	Errors() <-chan error
	Close() error
}
