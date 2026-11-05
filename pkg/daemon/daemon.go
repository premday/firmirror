// Package daemon runs firmirror as an application: it publishes at startup,
// publishes again when the rollout rings change, and refreshes on a schedule.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/premday/firmirror/pkg/firmirror"
)

// ErrPartial is what a refresh wraps when it published but something went wrong,
// such as a vendor failing. At startup it does not end the daemon: ending it would sign
// the metadata again at every restart.
var ErrPartial = errors.New("published with errors")

// ErrRetryable is what a failure wraps when asking again may get another answer,
// whatever else it is: a vendor that failed is one, which a refusal alongside does
// not make any less.
var ErrRetryable = errors.New("a new run may succeed")

// ErrUnchanged is what a regeneration returns when it found nothing to publish: it
// is no success of publishing, nor a failure.
var ErrUnchanged = errors.New("nothing to publish")

// ErrTimedOut is what a run that was stopped for taking longer than its timeout
// wraps.
var ErrTimedOut error = timedOut{}

type timedOut struct{}

func (timedOut) Error() string { return "run timed out" }

func (timedOut) Is(target error) bool { return target == ErrRetryable }

// RefreshResult is what a refresh reports besides its error.
type RefreshResult struct {
	NewComponents int
	// Interrupted is a refresh that ended before it was done mirroring, which is
	// what a timeout may have done: one that only ran long enough to publish after
	// its time was up was not stopped.
	Interrupted bool
}

// Daemon publishes the metadata and keeps it current. Refresh and regeneration
// run one at a time, on a single goroutine, as the store has one writer.
type Daemon struct {
	// Refresh mirrors the vendors and publishes. When always is set the metadata is
	// written and signed even if it did not change.
	Refresh func(ctx context.Context, always bool) (RefreshResult, error)
	// Regen publishes the stored metadata again, rendered with the definitions as
	// they are when it runs, not as they were when the Watcher saw them change.
	Regen func(ctx context.Context) error
	// Watcher tells when the definitions change. Nil when there are none.
	Watcher *Watcher
	// RefreshInterval is the time from the end of a refresh to the next one.
	RefreshInterval time.Duration
	// RetryInterval is the time from a failed regeneration to its retry, and from a
	// refresh that failed or published with errors to the next one. It doubles each
	// time the same kind of run fails again, up to RefreshInterval.
	RetryInterval time.Duration
	// RefreshTimeout and RegenTimeout stop a run that takes longer, as one that
	// hangs holds up every publish after it. A refresh stopped is published as far
	// as it got, and resumed after RetryInterval, which does not grow; a
	// regeneration is tried again like one that failed. 0 sets none.
	RefreshTimeout, RegenTimeout time.Duration
	Metrics                      *Metrics
	// MetricsAddress is where to serve the metrics. Empty serves none.
	MetricsAddress string

	// Runs of a kind that failed in a row, which the retry waits longer for.
	refreshFailures, regenFailures int
}

// Validate fails on an interval that is not positive, which would panic a ticker
// or make a loop that never waits.
func (d *Daemon) Validate() error {
	switch {
	case d.RefreshInterval <= 0:
		return fmt.Errorf("the refresh interval must be positive, got %s", d.RefreshInterval)
	case d.RetryInterval <= 0:
		return fmt.Errorf("the retry interval must be positive, got %s", d.RetryInterval)
	case d.RefreshTimeout < 0 || d.RegenTimeout < 0:
		return errors.New("the timeouts must not be negative")
	case d.Watcher != nil && d.Watcher.PollInterval <= 0:
		return fmt.Errorf("the poll interval must be positive, got %s", d.Watcher.PollInterval)
	}
	return nil
}

// Run returns an error when the first refresh cannot publish, and nil once the
// context is done and the run in progress has ended.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.Metrics == nil {
		d.Metrics = NewMetrics()
	}
	if d.Watcher != nil {
		d.Watcher.Metrics = d.Metrics
	}
	if d.MetricsAddress != "" {
		// Served until Run returns, not until the first signal: a publish in progress
		// goes on, and its state is worth scraping.
		serving, stopServing := context.WithCancel(context.WithoutCancel(ctx))
		defer stopServing()
		if err := d.Metrics.Serve(serving, d.MetricsAddress); err != nil {
			return err
		}
	}

	// Watch before the first refresh, which can take hours: a change meanwhile is
	// queued, not missed.
	updates := make(chan struct{}, 1)
	if d.Watcher != nil {
		watching, stopWatching := context.WithCancel(ctx)
		defer stopWatching()
		go d.Watcher.Run(watching, updates)
	}

	if ctx.Err() != nil {
		return nil
	}
	slog.Info("Starting: refreshing, and publishing the metadata whether or not it changed")
	err := d.refresh(ctx, true)
	if err != nil && !errors.Is(err, ErrPartial) {
		return fmt.Errorf("first refresh: %w", err)
	}

	timer := time.NewTimer(d.afterRefresh(err))
	defer timer.Stop()
	var retry <-chan time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			// Ready together with the end, it is not to start anything.
			if ctx.Err() != nil {
				return nil
			}
			timer.Reset(d.afterRefresh(d.refresh(ctx, false)))
		case <-updates:
			if ctx.Err() != nil {
				return nil
			}
			retry = d.regen(ctx)
		case <-retry:
			if ctx.Err() != nil {
				return nil
			}
			retry = d.regen(ctx)
		}
	}
}

// afterRefresh is the time to the next refresh. One that failed, or published with
// errors, is tried again sooner than a whole interval, as nothing else repairs
// what it left undone. A refusal alone is not: asking again gets the same answer,
// and each run mirrors again.
func (d *Daemon) afterRefresh(err error) time.Duration {
	switch {
	case err == nil:
		d.refreshFailures = 0
		return d.RefreshInterval
	case errors.Is(err, firmirror.ErrRefused) && !errors.Is(err, ErrRetryable) && !errors.Is(err, firmirror.ErrCleanup):
		d.refreshFailures = 0
		slog.Error("Refresh was refused something, trying again at the next interval", "error", err, "in", d.RefreshInterval)
		return d.RefreshInterval
	}
	if errors.Is(err, ErrTimedOut) && errors.Is(err, ErrPartial) {
		// It published what it mirrored, and is resumed: not a failure that more
		// waiting would help.
		d.refreshFailures = 0
		retry := min(d.RetryInterval, d.RefreshInterval)
		slog.Error("Refresh was stopped for its timeout, trying again", "error", err, "in", retry)
		return retry
	}
	d.refreshFailures++
	retry := d.backoff(d.refreshFailures)
	slog.Error("Refresh failed or published with errors, trying again", "error", err, "in", retry)
	return retry
}

// withTimeout is the context of a run, which ends after timeout if it is positive.
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// stopped reports whether the run ended for its timeout, not because it was told
// to end.
func stopped(parent, run context.Context) bool {
	return parent.Err() == nil && errors.Is(run.Err(), context.DeadlineExceeded)
}

// backoff is the wait after a run of a kind failed this many times in a row.
func (d *Daemon) backoff(failures int) time.Duration {
	wait := d.RetryInterval
	for i := 1; i < failures && wait < d.RefreshInterval; i++ {
		if wait > d.RefreshInterval/2 {
			return d.RefreshInterval // Doubling it would pass the interval, or overflow.
		}
		wait *= 2
	}
	return min(wait, d.RefreshInterval)
}

func (d *Daemon) refresh(ctx context.Context, force bool) error {
	d.Metrics.running.WithLabelValues("refresh").Set(1)
	defer d.Metrics.running.WithLabelValues("refresh").Set(0)
	start := time.Now()
	runCtx, cancel := withTimeout(ctx, d.RefreshTimeout)
	result, err := d.Refresh(runCtx, force)
	// Stopped before it mirrored, loading what is stored, is one too.
	cut := (result.Interrupted || errors.Is(err, context.DeadlineExceeded)) && stopped(ctx, runCtx)
	cancel()
	if cut {
		stoppedErr := fmt.Errorf("%w: the refresh took longer than %s and was stopped", ErrTimedOut, d.RefreshTimeout)
		if err == nil || errors.Is(err, ErrPartial) {
			// What it mirrored is published, and the rest is for the next run.
			stoppedErr = fmt.Errorf("%w: %w", ErrPartial, stoppedErr)
		}
		err = errors.Join(stoppedErr, err)
	}
	if ctx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)) {
		// Told to end: the run stopped early on purpose, and what it published is
		// not all of a refresh. A publish that failed meanwhile is a failure, which
		// this is not.
		return nil
	}
	d.Metrics.refreshDuration.Observe(time.Since(start).Seconds())
	d.Metrics.refreshNew.Set(float64(result.NewComponents))
	switch {
	case err == nil:
		d.Metrics.refreshRuns.WithLabelValues(resultSuccess).Inc()
		d.Metrics.refreshLastSuccess.SetToCurrentTime()
	case errors.Is(err, ErrPartial):
		d.Metrics.refreshRuns.WithLabelValues(resultPartial).Inc()
	default:
		d.Metrics.refreshRuns.WithLabelValues(resultFailure).Inc()
	}
	return err
}

// regen publishes the metadata for the definitions as they are now. It returns a
// channel that fires when to try again, or nil when there is nothing to retry.
func (d *Daemon) regen(ctx context.Context) <-chan time.Time {
	d.Metrics.running.WithLabelValues("regen").Set(1)
	defer d.Metrics.running.WithLabelValues("regen").Set(0)
	slog.Info("Regenerating the metadata for the changed rollout rings")
	runCtx, cancel := withTimeout(ctx, d.RegenTimeout)
	err := d.Regen(runCtx)
	if errors.Is(err, context.DeadlineExceeded) && stopped(ctx, runCtx) {
		err = errors.Join(fmt.Errorf("%w: the regeneration took longer than %s and was stopped", ErrTimedOut, d.RegenTimeout), err)
	}
	cancel()
	switch {
	case errors.Is(err, ErrUnchanged):
		d.regenFailures = 0
		d.Metrics.regenRuns.WithLabelValues(resultUnchanged).Inc()
		return nil
	case err == nil:
		d.regenFailures = 0
		d.Metrics.regenRuns.WithLabelValues(resultSuccess).Inc()
		d.Metrics.regenLastSuccess.SetToCurrentTime()
		return nil
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		return nil
	}
	d.Metrics.regenRuns.WithLabelValues(resultFailure).Inc()
	if errors.Is(err, firmirror.ErrRingsRefused) {
		d.regenFailures = 0
		// Asking again gets the same answer until the rings change, which the
		// watcher tells.
		slog.Error("Refused to publish the metadata for the changed rollout rings, not trying again until they change", "error", err)
		return nil
	}
	d.regenFailures++
	retry := d.backoff(d.regenFailures)
	slog.Error("Could not publish the metadata for the changed rollout rings, trying again", "error", err, "in", retry)
	return time.After(retry)
}
