//go:build linux

package watching

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rjeczalik/notify"
)

var RecursiveWatchingSupported = true

func NewRecursiveWatcher(target string) (RecursiveWatcher, error) {
	slog.Debug("making recursive watcher", "target", target)
	notifyChan := make(chan notify.EventInfo, 1)
	if err := notify.Watch(target, notifyChan, notify.All); err != nil {
		return nil, err
	}

	watcher := &LinuxRecursiveWatcher{
		notifyChan: notifyChan,
		closeCh:    make(chan struct{}),
		eventsCh:   make(chan string),
		errorsCh:   make(chan error),
	}
	watcher.Start(target)

	return watcher, nil
}

type LinuxRecursiveWatcher struct {
	notifyChan    chan notify.EventInfo
	activeWorkers sync.WaitGroup
	closeCh       chan struct{}
	started       atomic.Bool
	terminated    atomic.Bool

	eventsCh chan string
	// TODO(erd): unused?
	errorsCh chan error
}

func (w *LinuxRecursiveWatcher) Start(target string) {
	if w.terminated.Load() {
		return
	}
	if !w.started.CompareAndSwap(false, true) {
		return
	}
	w.activeWorkers.Add(1)
	go func() {
		defer w.activeWorkers.Done()

		for {
			var eventInfo notify.EventInfo
			select {
			case <-w.closeCh:
				return
			case eventInfo = <-w.notifyChan:
			}

			slog.Debug("got event", "path", eventInfo.Path(), "event", eventInfo.Event(), "sys", eventInfo.Sys())
			select {
			case <-w.closeCh:
				return
			// mutagen likes root-relative paths
			case w.eventsCh <- strings.TrimPrefix(eventInfo.Path(), target+"/"):
			}
			continue
		}
	}()
}

func (w *LinuxRecursiveWatcher) Events() <-chan string {
	return w.eventsCh

}

func (w *LinuxRecursiveWatcher) Errors() <-chan error {
	return w.errorsCh
}

func (w *LinuxRecursiveWatcher) Terminate() error {
	if !w.started.CompareAndSwap(false, true) {
		return nil
	}
	notify.Stop(w.notifyChan)
	close(w.closeCh)
	w.activeWorkers.Wait()
	close(w.eventsCh)
	close(w.errorsCh)
	return nil
}
