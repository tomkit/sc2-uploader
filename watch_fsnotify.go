//go:build !darwin

package main

import (
	"github.com/fsnotify/fsnotify"
)

// Windows (ReadDirectoryChangesW) and Linux (inotify): one handle per
// watched folder.
type fsnotifyWatcher struct {
	w     *fsnotify.Watcher
	paths chan string
	errs  chan error
}

func newDirWatcher() (dirWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &fsnotifyWatcher{w: w, paths: make(chan string, 256), errs: make(chan error, 4)}
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					close(fw.paths)
					return
				}
				if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
					fw.paths <- ev.Name
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				select {
				case fw.errs <- err:
				default:
				}
			}
		}
	}()
	return fw, nil
}

func (f *fsnotifyWatcher) Add(dir string) error { return f.w.Add(dir) }
func (f *fsnotifyWatcher) Paths() <-chan string { return f.paths }
func (f *fsnotifyWatcher) Errors() <-chan error { return f.errs }
func (f *fsnotifyWatcher) Close() error         { return f.w.Close() }
