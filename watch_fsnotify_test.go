//go:build !darwin

package main

import (
	"errors"
	"testing"
)

func TestWatcherRequestsRescanWhenEventsOverflow(t *testing.T) {
	w := &fsnotifyWatcher{paths: make(chan string, 1), errs: make(chan error, 1)}
	w.publish("first.SC2Replay")
	w.publish("second.SC2Replay")
	if got := <-w.errs; !errors.Is(got, errDropped) {
		t.Fatalf("full watcher queue should request a rescan, got %v", got)
	}
}
