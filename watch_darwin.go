package main

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

// FSEvents: one kernel-coalesced stream for every watched root, recursive,
// without opening the files (kqueue, fsnotify's macOS backend, holds a
// descriptor per file — thousands for a big replay folder).
type fseventsWatcher struct {
	mu     sync.Mutex
	roots  []string
	stream *fsevents.EventStream
	paths  chan string
	errs   chan error
	stop   chan struct{}
}

func newDirWatcher() (dirWatcher, error) {
	return &fseventsWatcher{paths: make(chan string, 256), errs: make(chan error, 4), stop: make(chan struct{})}, nil
}

func (w *fseventsWatcher) Paths() <-chan string { return w.paths }
func (w *fseventsWatcher) Errors() <-chan error { return w.errs }

func covers(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *fseventsWatcher) Add(dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.roots {
		if covers(r, dir) {
			return nil
		}
	}
	w.roots = append(w.roots, dir)
	return w.restartLocked()
}

// restartLocked replaces the stream with one over every root.
func (w *fseventsWatcher) restartLocked() error {
	if w.stream != nil {
		w.stream.Stop()
	}
	es := &fsevents.EventStream{
		Paths:   append([]string{}, w.roots...),
		Latency: 500 * time.Millisecond,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	if err := es.Start(); err != nil {
		return err
	}
	w.stream = es
	go func(events chan []fsevents.Event) {
		for {
			select {
			case <-w.stop:
				return
			case batch, ok := <-events:
				if !ok {
					return
				}
				for _, ev := range batch {
					if ev.Flags&(fsevents.MustScanSubDirs|fsevents.KernelDropped|fsevents.UserDropped|fsevents.RootChanged) != 0 {
						select {
						case w.errs <- errDropped:
						default:
						}
						continue
					}
					path := ev.Path
					if !filepath.IsAbs(path) {
						path = "/" + path
					}
					select {
					case w.paths <- path:
					case <-w.stop:
						return
					}
				}
			}
		}
	}(es.Events)
	return nil
}

func (w *fseventsWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	if w.stream != nil {
		w.stream.Stop()
		w.stream = nil
	}
	return nil
}
