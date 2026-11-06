package daemon

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/utils"
)

// loadFunc reads the definitions and the digest of what it read.
type loadFunc func(ctx context.Context, source string, allowHTTP bool) (*bkc.File, [sha256.Size]byte, error)

// Watcher tells when the rollout ring definitions change. A source that is a path
// is watched for changes, and every source is also read every PollInterval: that
// is all a URL has, and for a path it covers events a filesystem does not deliver.
// A change is the SHA-256 of the content, so an event that changed nothing is no
// change.
type Watcher struct {
	Source       string
	AllowHTTP    bool
	PollInterval time.Duration
	// Debounce is how long events are gathered before the source is read, as an
	// editor or a volume update writes a file in several steps.
	Debounce time.Duration
	// Seen is the digest of the definitions the daemon started with.
	Seen [sha256.Size]byte
	// File is what the daemon read of it, which a later edit is compared with.
	File    *bkc.File
	Metrics *Metrics

	load loadFunc
	// file is what was read when the content last changed, to tell an edit that
	// changes nothing the rings render, as one of a comment, from a change.
	file *bkc.File
}

func (w *Watcher) seed() {
	if w.file == nil {
		w.file = w.File
	}
	w.File = nil
}

// signal sends on a channel unless a signal is already waiting in it.
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run signals on updates when the definitions change, until the context is done.
// A change while one is still to be received adds nothing to it. It reads them
// once first, so that the metrics tell the daemon started.
func (w *Watcher) Run(ctx context.Context, updates chan<- struct{}) {
	if w.load == nil {
		w.load = bkc.LoadWithDigest
	}
	if w.Debounce <= 0 {
		w.Debounce = 500 * time.Millisecond
	}
	if w.Metrics == nil {
		w.Metrics = NewMetrics()
	}
	w.seed()
	events, closeEvents, watchLinked := w.watchPath()
	defer closeEvents()

	onDisk := w.stat()
	if !w.check(ctx, updates) {
		onDisk = nil // Not read: the first event tries again.
	}
	// recheck reads the definitions for a file that is now as given, which is kept as
	// the file seen only if they could be read: the next event, or the poll, tries
	// again otherwise.
	recheck := func(now os.FileInfo) {
		previous := onDisk
		onDisk = now
		if !w.check(ctx, updates) {
			onDisk = previous
		}
		watchLinked()
	}
	poll := time.NewTicker(w.PollInterval)
	defer poll.Stop()
	var debounce *time.Timer
	var debounced <-chan time.Time
	var burst time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			// Looked at before reading, as an edit in between is then seen as one.
			recheck(w.stat())
		case <-events:
			// Events keep it waiting, but not for long: a file written to all the time
			// next to the definitions must not hide a change.
			switch {
			case debounce == nil:
				debounce = time.NewTimer(w.Debounce)
				debounced = debounce.C
				burst = time.Now()
			case time.Since(burst) < 10*w.Debounce:
				debounce.Reset(w.Debounce)
			}
		case <-debounced:
			debounce, debounced = nil, nil
			// An event of another file of the directory leaves this one as it was.
			if now := w.stat(); (now != nil || onDisk != nil) && (now == nil || onDisk == nil || !os.SameFile(onDisk, now) || !now.ModTime().Equal(onDisk.ModTime()) || now.Size() != onDisk.Size()) {
				recheck(now)
			}
		}
	}
}

// watchPath watches the directory of a source that is a path, not the file: an
// editor replaces a file by renaming another over it, and a volume built from a
// ConfigMap updates it by swapping a symlink next to it, neither of which an
// event on the file itself shows. It returns a nil channel for a URL, or when the
// watch cannot be set up, which the polling then covers.
func (w *Watcher) watchPath() (events <-chan struct{}, closeWatch func(), watchLinked func()) {
	path, local := utils.LocalPath(w.Source)
	if !local {
		return nil, func() {}, func() {}
	}
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		if err = watcher.Add(filepath.Dir(path)); err != nil {
			watcher.Close()
		}
	}
	if err != nil {
		slog.Warn("Cannot watch the rollout rings for changes, reading them every poll interval instead", "error", err, "interval", w.PollInterval)
		return nil, func() {}, func() {}
	}
	slog.Info("Watching the rollout rings", "file", path)

	// What a link points at changes in its own directory, which can be another one
	// after a directory of the path is swapped for another link: it is looked at
	// again after each check.
	watchLinked = func() {
		if real, linkErr := bkc.FollowLinks(path); linkErr == nil && filepath.Dir(real) != filepath.Dir(path) {
			if addErr := watcher.Add(filepath.Dir(real)); addErr != nil {
				slog.Warn("Cannot watch the directory the rollout rings file links to, so a change there waits for the next read", "directory", filepath.Dir(real), "error", addErr)
			}
		}
	}
	watchLinked()

	changes := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case _, ok := <-watcher.Events:
				if !ok {
					return
				}
				signal(changes)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Warn("Error watching the rollout rings", "error", err)
				// An event may have been lost, which only a read tells.
				signal(changes)
			}
		}
	}()
	return changes, func() {
		close(done)
		watcher.Close()
	}, watchLinked
}

// stat is what the file system says of the rings file, or nil for a URL or a file
// that cannot be told about.
func (w *Watcher) stat() os.FileInfo {
	path, local := utils.LocalPath(w.Source)
	if !local {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// check reads the definitions, and reports whether it could.
func (w *Watcher) check(ctx context.Context, updates chan<- struct{}) bool {
	file, digest, err := w.load(ctx, w.Source, w.AllowHTTP)
	if err != nil {
		if ctx.Err() == nil {
			// The rings being served stay as they are.
			slog.Warn("Could not read the rollout rings, keeping the ones in use", "error", err)
			w.Metrics.bkcChecks.WithLabelValues(resultError).Inc()
		}
		return false
	}
	w.Metrics.bkcLastCheck.SetToCurrentTime()
	if digest == w.Seen {
		if w.file == nil {
			w.file = file
		}
		w.Metrics.bkcChecks.WithLabelValues(resultUnchanged).Inc()
		return true
	}
	previous := w.file
	w.Seen, w.file = digest, file
	if previous != nil && previous.SameServing(file) {
		// Publishing again would load the whole stored metadata to render the same.
		w.Metrics.bkcChecks.WithLabelValues(resultUnchanged).Inc()
		return true
	}
	w.Metrics.bkcChecks.WithLabelValues(resultChanged).Inc()
	w.Metrics.bkcLastChange.SetToCurrentTime()
	slog.Info("The rollout rings changed")

	signal(updates) // A signal already waiting is as good.
	return true
}
