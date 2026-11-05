package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/daemon"
	"github.com/premday/firmirror/pkg/firmirror"
)

// daemonError is the error of a refresh as the daemon takes it. A refresh that
// left a document in the store but went wrong around it is partial, which does not
// end the daemon at startup: a vendor failed, removing unreferenced packages did,
// the mirrored components were not published, or other rings than the ones given
// were, as those were refused. One that left no document is a failure, as there is
// nothing to serve.
func daemonError(outcome refreshOutcome, err error, told bool) error {
	var problems []error
	if outcome.vendorsFailed {
		problems = append(problems, errVendorsFailed)
	}
	if outcome.fellBack {
		problems = append(problems, errRingsRefused)
	}
	if err == nil && outcome.newComponents > 0 && !outcome.published && !told {
		problems = append(problems, errNotPublished)
	}
	switch {
	case err == nil && !outcome.holdsMetadata && !told:
		// Nothing was mirrored, and nothing is stored: an empty mirror is not a success.
		return cmp.Or(errors.Join(problems...), errNothingToPublish)
	case err == nil && len(problems) > 0:
		return fmt.Errorf("%w: %w", daemon.ErrPartial, errors.Join(problems...))
	case errors.Is(err, firmirror.ErrCleanup):
		return fmt.Errorf("%w: %w", daemon.ErrPartial, errors.Join(append([]error{err}, problems...)...))
	case err != nil && outcome.vendorsFailed:
		return errors.Join(append([]error{err}, problems...)...)
	}
	return err
}

var (
	errNothingToPublish = errors.New("nothing was mirrored and there is no metadata to publish: check the vendors enabled and what they are asked for")
	// It matches ErrRefused: asking again gets the same answer until the rings change.
	errRingsRefused = fmt.Errorf("the rollout rings were refused, so others were published: %w", firmirror.ErrRefused)
)

// ringsInForce reads the rollout rings again. When they cannot be read the ones in
// use are kept, as the watcher does: a bad commit must not stop the mirroring.
func ringsInForce(ctx context.Context, inUse *bkc.File) *bkc.File {
	if args.BKC.File == "" {
		return nil
	}
	fresh, err := loadDefinitions(ctx)
	if err != nil {
		slog.Warn("Could not read the rollout rings, refreshing with the ones in use", "error", err)
		return inUse
	}
	return fresh
}

// runDaemon keeps the mirror published until it is told to end.
func runDaemon(ctx context.Context) (err error) {
	// Told to end before it started anything is no failure.
	defer func() {
		if errors.Is(err, context.Canceled) {
			err = nil
		}
	}()
	flags := args.Daemon.DaemonFlags
	overrides := args.Daemon.Overrides
	if err := checkRefreshArgs(); err != nil {
		return err
	}
	d := &daemon.Daemon{
		RefreshInterval: flags.RefreshInterval,
		RetryInterval:   flags.RetryInterval,
		MetricsAddress:  flags.MetricsAddress,
		RefreshTimeout:  flags.RefreshTimeout,
		RegenTimeout:    flags.RegenTimeout,
	}
	if args.BKC.File != "" {
		d.Watcher = &daemon.Watcher{PollInterval: flags.BKCPollInterval}
	}
	// Before anything is opened or removed.
	if err := d.Validate(); err != nil {
		return err
	}
	var rings *bkc.File
	if args.BKC.File != "" {
		// Fail on rings that cannot be read before serving anything, or opening the
		// storage, which removes the temporaries of a killed write, as a refresh does.
		file, digest, err := bkc.LoadWithDigest(ctx, args.BKC.File, args.BKC.AllowHTTP)
		if err != nil {
			return allowHTTPHint(err)
		}
		rings = file
		d.Watcher = &daemon.Watcher{
			Source:       args.BKC.File,
			AllowHTTP:    args.BKC.AllowHTTP,
			PollInterval: flags.BKCPollInterval,
			Seen:         digest,
			File:         file,
		}
	} else {
		slog.Warn("No --bkc.file: no ring tag is published, and there are no rings to watch")
	}

	storage, err := openWritableStorage(ctx)
	if err != nil {
		return err
	}

	// Regenerations share a cache directory, so a second signal can remove it.
	cacheDir, err := os.MkdirTemp("", "firmirror-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cacheDir)

	// The rings in force are the ones last published, or read at startup, which the
	// first refresh publishes and the watcher starts from. A refresh or a
	// regeneration reads them again, and falls back to these when the new ones
	// cannot be read or would be refused.
	started := false
	d.Refresh = func(ctx context.Context, always bool) (daemon.RefreshResult, error) {
		definitions := rings
		if started {
			// Not at the first, as opening the storage did.
			removeStaleTemporaries(storage)
			definitions = ringsInForce(ctx, rings)
		}
		started = true
		outcome, err := refresh(ctx, storage, definitions, refreshRun{overrides: overrides, alwaysPublish: always, quietStop: true, inUse: rings})
		err = daemonError(outcome, err, errors.Is(ctx.Err(), context.Canceled))
		if outcome.published {
			rings = outcome.rings
		}
		return daemon.RefreshResult{NewComponents: outcome.newComponents, Interrupted: outcome.interrupted}, err
	}
	d.Regen = func(ctx context.Context) error {
		// The rings as they are now, not as the watcher saw them: a refresh may
		// have published newer ones since.
		definitions, err := loadDefinitions(ctx)
		if err != nil {
			return err
		}
		if rings != nil && definitions.SameServing(rings) {
			slog.Info("The rollout rings serve what the last published ones do")
			return daemon.ErrUnchanged
		}
		syncer, err := syncerOn(ctx, publishConfig(definitions, overrides), storage, cacheDir)
		if err != nil {
			return err
		}
		// A signal during the load asks not to start a publish.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncer.RegenerateMetadataIfChanged(ctx); err != nil {
			return err
		}
		rings = definitions
		return nil
	}

	// A publish that has begun is finished: the first signal stops the daemon from
	// starting anything, and only a second one ends the process mid-publish.
	endOnSecondSignal(ctx, func() { os.RemoveAll(cacheDir) })
	return d.Run(ctx)
}
