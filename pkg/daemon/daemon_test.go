package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/firmirror"
)

// recorder is a Refresh and a Regen that log what they were asked and that
// notice when two run at once.
type recorder struct {
	mu      sync.Mutex
	calls   []string
	active  atomic.Int32
	overlap atomic.Bool

	refreshErr  []error // returned by successive refreshes, then nil
	regenErr    []error
	refreshWork func()
}

func (r *recorder) log(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) next(errs *[]error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (r *recorder) enter() func() {
	if r.active.Add(1) > 1 {
		r.overlap.Store(true)
	}
	return func() { r.active.Add(-1) }
}

func (r *recorder) refresh(_ context.Context, force bool) (RefreshResult, error) {
	defer r.enter()()
	if force {
		r.log("refresh-forced")
	} else {
		r.log("refresh")
	}
	if r.refreshWork != nil {
		r.refreshWork()
	}
	return RefreshResult{NewComponents: 3}, r.next(&r.refreshErr)
}

func (r *recorder) regen(context.Context) error {
	defer r.enter()()
	r.log("regen")
	return r.next(&r.regenErr)
}

// ringsNamed is rings whose content tells them apart.
func ringsNamed(prefix string) *bkc.File {
	file := bkc.New([]string{"dev"})
	file.TagPrefix = prefix
	return file
}

// changing is a loader that reports other rings each time it is asked.
func changing(calls *atomic.Int32) loadFunc {
	return func(context.Context, string, bool) (*bkc.File, [32]byte, error) {
		n := calls.Add(1)
		return ringsNamed(fmt.Sprint("p", n)), [32]byte{byte(n)}, nil
	}
}

// changedOnce is a loader that reports one digest that is not the seen one.
func changedOnce(context.Context, string, bool) (*bkc.File, [32]byte, error) {
	return bkc.New([]string{"dev"}), [32]byte{1}, nil
}

func start(t *testing.T, d *Daemon) (done chan error, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(cancel)
	return done, cancel
}

func eventually(t *testing.T, condition func() bool, what string) {
	t.Helper()
	require.Eventually(t, condition, 5*time.Second, 5*time.Millisecond, what)
}

func newDaemon(r *recorder, interval time.Duration) *Daemon {
	return &Daemon{
		Refresh:         r.refresh,
		Regen:           r.regen,
		RefreshInterval: interval,
		RetryInterval:   10 * time.Millisecond,
		Metrics:         NewMetrics(),
	}
}

func TestFirstRefreshIsForcedAndTheNextOnesAreNot(t *testing.T) {
	r := &recorder{}
	d := newDaemon(r, 20*time.Millisecond)
	start(t, d)

	eventually(t, func() bool { return len(r.seen()) >= 3 }, "refreshes")
	calls := r.seen()
	assert.Equal(t, "refresh-forced", calls[0])
	assert.Equal(t, "refresh", calls[1])
	assert.Equal(t, "refresh", calls[2])
	assert.Equal(t, 3.0, testutil.ToFloat64(d.Metrics.refreshNew))
	assert.Greater(t, testutil.ToFloat64(d.Metrics.refreshLastSuccess), 0.0)
}

func TestFirstRefreshFailureEndsTheDaemonButAPartialOneDoesNot(t *testing.T) {
	r := &recorder{refreshErr: []error{errors.New("cannot publish")}}
	done, _ := start(t, newDaemon(r, time.Hour))
	select {
	case err := <-done:
		assert.ErrorContains(t, err, "cannot publish")
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon should have ended")
	}

	r = &recorder{refreshErr: []error{ErrPartial}}
	d := newDaemon(r, time.Hour)
	done, stop := start(t, d)
	eventually(t, func() bool { return testutil.ToFloat64(d.Metrics.refreshRuns.WithLabelValues(resultPartial)) == 1 }, "partial counted")
	stop()
	assert.NoError(t, <-done)
}

func TestLaterRefreshFailuresAreCountedAndTheRefreshGoesOn(t *testing.T) {
	r := &recorder{refreshErr: []error{nil, errors.New("vendor down")}}
	d := newDaemon(r, 10*time.Millisecond)
	done, stop := start(t, d)

	eventually(t, func() bool { return len(r.seen()) >= 4 }, "refreshes go on after a failure")
	stop()
	assert.NoError(t, <-done)
	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.refreshRuns.WithLabelValues(resultFailure)))
}

func TestAChangeOfTheRingsRegenerates(t *testing.T) {
	r := &recorder{}
	d := newDaemon(r, time.Hour)
	d.Watcher = &Watcher{Source: "unused", PollInterval: 5 * time.Millisecond, load: changedOnce}
	start(t, d)

	eventually(t, func() bool { return len(r.seen()) == 2 }, "regen")
	assert.Equal(t, []string{"refresh-forced", "regen"}, r.seen())
	// The same content is not a change again.
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, r.seen(), 2)
	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultSuccess)))
}

func TestAChangeDuringARefreshWaitsForItAndNothingOverlaps(t *testing.T) {
	release := make(chan struct{})
	r := &recorder{refreshWork: func() { <-release }}
	d := newDaemon(r, time.Hour)
	d.Watcher = &Watcher{Source: "unused", PollInterval: 5 * time.Millisecond, load: changedOnce}
	start(t, d)

	// The watcher sees the change while the first refresh is going on.
	eventually(t, func() bool {
		return testutil.ToFloat64(d.Metrics.bkcChecks.WithLabelValues(resultChanged)) == 1
	}, "change seen")
	assert.Equal(t, []string{"refresh-forced"}, r.seen(), "the regeneration waits for the refresh")
	close(release)
	eventually(t, func() bool { return len(r.seen()) == 2 }, "regen after the refresh")
	assert.False(t, r.overlap.Load())
}

func TestChangesDuringARefreshAreOneRegeneration(t *testing.T) {
	release := make(chan struct{})
	r := &recorder{refreshWork: func() { <-release }}
	d := newDaemon(r, time.Hour)
	var reads atomic.Int32
	d.Watcher = &Watcher{Source: "unused", PollInterval: 5 * time.Millisecond, load: changing(&reads)}
	start(t, d)

	eventually(t, func() bool { return reads.Load() >= 4 }, "several changes seen")
	close(release)
	eventually(t, func() bool { return len(r.seen()) >= 2 }, "regen after the refresh")
	assert.Equal(t, "regen", r.seen()[1], "the regeneration reads the definitions itself, so there is nothing stale to publish")
}

func TestAFailedRegenerationIsRetried(t *testing.T) {
	r := &recorder{regenErr: []error{errors.New("no metadata yet"), errors.New("still none")}}
	d := newDaemon(r, time.Hour)
	d.Watcher = &Watcher{Source: "unused", PollInterval: 5 * time.Millisecond, load: changedOnce}
	start(t, d)

	eventually(t, func() bool { return testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultSuccess)) == 1 }, "regen succeeds")
	assert.Equal(t, 2.0, testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultFailure)))
	assert.Equal(t, []string{"refresh-forced", "regen", "regen", "regen"}, r.seen())
}

func TestARefusedRegenerationIsNotRetriedUntilTheRingsChange(t *testing.T) {
	r := &recorder{regenErr: []error{fmt.Errorf("would drop a ring: %w", firmirror.ErrRingsRefused)}}
	d := newDaemon(r, time.Hour)
	var reads atomic.Int32
	d.Watcher = &Watcher{Source: "unused", PollInterval: 5 * time.Millisecond, load: func(ctx context.Context, source string, allowHTTP bool) (*bkc.File, [32]byte, error) {
		// One change, then the same content.
		if reads.Add(1) == 1 {
			return changedOnce(ctx, source, allowHTTP)
		}
		return bkc.New([]string{"dev"}), [32]byte{1}, nil
	}}
	start(t, d)

	eventually(t, func() bool { return testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultFailure)) == 1 }, "regen refused")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, []string{"refresh-forced", "regen"}, r.seen(), "asking again would be refused again")
}

func TestAChangeReplacesTheRetryOfAFailedRegeneration(t *testing.T) {
	r := &recorder{regenErr: []error{errors.New("no metadata yet")}}
	d := newDaemon(r, time.Hour)
	d.RetryInterval = time.Hour
	var reads atomic.Int32
	d.Watcher = &Watcher{Source: "unused", PollInterval: 20 * time.Millisecond, load: func(ctx context.Context, source string, allowHTTP bool) (*bkc.File, [32]byte, error) {
		// A change at the first read and another later.
		n := min(reads.Add(1), 3)
		return ringsNamed(fmt.Sprint("p", n)), [32]byte{byte(n)}, nil
	}}
	start(t, d)

	eventually(t, func() bool { return testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultSuccess)) == 1 }, "the later change is published without waiting for the retry")
}

func TestAFailedOrPartialRefreshIsTriedAgainSoonerAndLessOftenEachTime(t *testing.T) {
	d := &Daemon{RefreshInterval: time.Hour, RetryInterval: 10 * time.Minute}

	assert.Equal(t, time.Hour, d.afterRefresh(nil))
	assert.Equal(t, 10*time.Minute, d.afterRefresh(ErrPartial), "a vendor that was down a moment is not left out for a whole interval")
	assert.Equal(t, 20*time.Minute, d.afterRefresh(errors.New("could not write")), "what keeps failing is not hammered")
	assert.Equal(t, 40*time.Minute, d.afterRefresh(ErrPartial))
	assert.Equal(t, time.Hour, d.afterRefresh(ErrPartial), "never later than the interval")
	assert.Equal(t, time.Hour, d.afterRefresh(fmt.Errorf("would drop a ring: %w", firmirror.ErrRefused)), "each run mirrors again, and would be refused again")
	assert.Equal(t, 10*time.Minute, d.afterRefresh(ErrPartial), "a refusal ends the series")
	assert.Equal(t, 20*time.Minute, d.afterRefresh(fmt.Errorf("%w: %w", ErrPartial, errors.Join(ErrRetryable, firmirror.ErrRefused))), "a vendor that failed is retried, refused rings or not")
	assert.Equal(t, 40*time.Minute, d.afterRefresh(fmt.Errorf("%w: %w", ErrPartial, errors.Join(firmirror.ErrCleanup, firmirror.ErrRefused))), "so is a cleanup")
	assert.Equal(t, 10*time.Minute, (&Daemon{RefreshInterval: time.Hour, RetryInterval: 10 * time.Minute}).afterRefresh(fmt.Errorf("%w: %w", ErrPartial, errors.Join(ErrTimedOut, firmirror.ErrRefused))), "so is a refresh that was stopped")
	assert.Equal(t, time.Hour, d.afterRefresh(nil))
	assert.Equal(t, 10*time.Minute, d.afterRefresh(ErrPartial), "and so does a success")
}

func TestTheBackoffNeverOverflowsOrExceedsTheRefreshInterval(t *testing.T) {
	d := &Daemon{RefreshInterval: 168 * time.Hour, RetryInterval: 48 * time.Hour}

	for failures := 1; failures <= 100; failures++ {
		assert.Positive(t, d.backoff(failures), failures)
		assert.LessOrEqual(t, d.backoff(failures), d.RefreshInterval, failures)
	}
	assert.Equal(t, 48*time.Hour, d.backoff(1))
	assert.Equal(t, 96*time.Hour, d.backoff(2))
	assert.Equal(t, 168*time.Hour, d.backoff(3))
}

func TestAStartupRefreshThatPublishedWithErrorsIsTriedAgainSoon(t *testing.T) {
	r := &recorder{refreshErr: []error{ErrPartial}}
	d := newDaemon(r, time.Hour)
	start(t, d)

	eventually(t, func() bool { return len(r.seen()) >= 2 }, "the next refresh does not wait for the interval")
	assert.Equal(t, []string{"refresh-forced", "refresh"}, r.seen()[:2], "only the first is forced")
}

func TestARegenerationThatFoundNothingToPublishIsNeitherASuccessNorAFailure(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.Regen = func(context.Context) error { return fmt.Errorf("the same rings: %w", ErrUnchanged) }

	assert.Nil(t, d.regen(context.Background()))

	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultUnchanged)))
	assert.Zero(t, testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultSuccess)))
	assert.Zero(t, testutil.ToFloat64(d.Metrics.regenLastSuccess))
}

func TestAContextThatIsDoneBeforeRunStartsNothing(t *testing.T) {
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, newDaemon(r, time.Hour).Run(ctx))

	assert.Empty(t, r.seen())
}

func TestARefreshCutShortByShutdownIsNotASuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newDaemon(&recorder{}, time.Hour)
	d.Refresh = func(context.Context, bool) (RefreshResult, error) {
		cancel()
		return RefreshResult{NewComponents: 1}, nil
	}

	require.NoError(t, d.Run(ctx))

	assert.Zero(t, testutil.ToFloat64(d.Metrics.refreshRuns.WithLabelValues(resultSuccess)))
	assert.Zero(t, testutil.ToFloat64(d.Metrics.refreshLastSuccess))
}

func TestAShutdownDuringAFailedRegenerationStillCountsTheFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newDaemon(&recorder{}, time.Hour)
	d.Regen = func(context.Context) error {
		cancel()
		return errors.New("storage error")
	}

	d.regen(ctx)

	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultFailure)))
}

func TestAnIntervalThatIsNotPositiveIsRefused(t *testing.T) {
	good := func() *Daemon {
		return &Daemon{RefreshInterval: time.Hour, RetryInterval: time.Minute, Watcher: &Watcher{PollInterval: time.Minute}}
	}
	require.NoError(t, good().Validate())
	for name, break_ := range map[string]func(*Daemon){
		"refresh": func(d *Daemon) { d.RefreshInterval = 0 },
		"retry":   func(d *Daemon) { d.RetryInterval = -time.Second },
		"poll":    func(d *Daemon) { d.Watcher.PollInterval = 0 },
	} {
		d := good()
		break_(d)
		assert.Error(t, d.Run(context.Background()), name)
	}
}

func TestAPublishThatFailedWhileTheDaemonWasToldToEndIsAFailure(t *testing.T) {
	for name, test := range map[string]struct {
		err    error
		failed bool
	}{
		"cancelled":     {err: fmt.Errorf("loading: %w", context.Canceled)},
		"publish error": {err: errors.New("could not publish"), failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			d := &Daemon{
				RefreshInterval: time.Hour, RetryInterval: time.Hour, Metrics: NewMetrics(),
				Refresh: func(context.Context, bool) (RefreshResult, error) {
					cancel()
					return RefreshResult{}, test.err
				},
				Regen: func(context.Context) error { return nil },
			}

			err := d.Run(ctx)

			if test.failed {
				assert.ErrorContains(t, err, "could not publish")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestARefreshThatFailedStillSaysWhatItMirrored(t *testing.T) {
	r := &recorder{refreshErr: []error{nil, errors.New("could not publish")}}
	d := newDaemon(r, 10*time.Millisecond)
	start(t, d)

	eventually(t, func() bool { return testutil.ToFloat64(d.Metrics.refreshRuns.WithLabelValues(resultFailure)) >= 1 }, "a failure")
	assert.Equal(t, 3.0, testutil.ToFloat64(d.Metrics.refreshNew))
}

func TestMetricsServesHealthAndTheMetrics(t *testing.T) {
	m := NewMetrics()
	server := httptest.NewServer(m.handler())
	defer server.Close()

	for path, want := range map[string]string{"/healthz": "ok", "/metrics": "firmirror_run_in_progress"} {
		response, err := http.Get(server.URL + path)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode, path)
		assert.Contains(t, string(body), want, path)
	}
}

func TestMetricsRefusesAnAddressInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	err = NewMetrics().Serve(context.Background(), listener.Addr().String())

	assert.ErrorContains(t, err, "metrics")
}

func TestARefreshThatTakesTooLongIsStoppedAndCountsAsPartial(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.RefreshTimeout = 20 * time.Millisecond
	d.Refresh = func(ctx context.Context, _ bool) (RefreshResult, error) {
		<-ctx.Done()
		return RefreshResult{Interrupted: true}, nil
	}

	err := d.refresh(context.Background(), false)

	assert.ErrorIs(t, err, ErrPartial, "what it mirrored is published")
	assert.ErrorIs(t, err, ErrTimedOut)
	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.refreshRuns.WithLabelValues(resultPartial)))
	assert.Less(t, d.afterRefresh(err), time.Hour, "tried again soon, as a failure is")
}

func TestARefreshThatTimedOutWithoutPublishingStaysAFailure(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.RefreshTimeout = 20 * time.Millisecond
	d.Refresh = func(ctx context.Context, _ bool) (RefreshResult, error) {
		<-ctx.Done()
		return RefreshResult{Interrupted: true}, errors.New("nothing to publish")
	}

	err := d.refresh(context.Background(), true)

	assert.NotErrorIs(t, err, ErrPartial, "the first refresh must not keep the daemon up on it")
	assert.ErrorIs(t, err, ErrTimedOut)
}

func TestARunToldToEndIsNotATimeout(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.RefreshTimeout = time.Hour
	d.Refresh = func(ctx context.Context, _ bool) (RefreshResult, error) {
		<-ctx.Done()
		return RefreshResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	assert.NoError(t, d.refresh(ctx, false))
}

func TestARegenerationThatTakesTooLongIsStoppedAndRetried(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.RegenTimeout = 20 * time.Millisecond
	d.Regen = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	retry := d.regen(context.Background())

	assert.NotNil(t, retry, "a timeout is a failure to try again")
	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.regenRuns.WithLabelValues(resultFailure)))
}

func TestARefreshThatOnlyPublishedPastItsTimeWasNotStopped(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.RefreshTimeout = 20 * time.Millisecond
	d.Refresh = func(ctx context.Context, _ bool) (RefreshResult, error) {
		<-ctx.Done() // Done mirroring, and publishing, which the time does not cut short.
		return RefreshResult{}, nil
	}

	assert.NoError(t, d.refresh(context.Background(), false))
	assert.Equal(t, 1.0, testutil.ToFloat64(d.Metrics.refreshRuns.WithLabelValues(resultSuccess)))
}

func TestARegenerationThatFailedForAnotherReasonPastItsTimeKeepsItsError(t *testing.T) {
	d := newDaemon(&recorder{}, time.Hour)
	d.RegenTimeout = 20 * time.Millisecond
	other := errors.New("could not write")
	d.Regen = func(ctx context.Context) error {
		<-ctx.Done()
		return other
	}

	// Observed through the retry, which a refusal would not get.
	assert.NotNil(t, d.regen(context.Background()))
}

func TestARefreshStoppedForItsTimeoutThatPublishedIsRetriedAtTheRetryInterval(t *testing.T) {
	d := &Daemon{RefreshInterval: time.Hour, RetryInterval: 10 * time.Minute}
	stopped := fmt.Errorf("%w: %w", ErrPartial, ErrTimedOut)

	assert.Equal(t, 10*time.Minute, d.afterRefresh(stopped))
	assert.Equal(t, 10*time.Minute, d.afterRefresh(stopped), "a mirror longer than the timeout is resumed without waiting longer each time")
}
