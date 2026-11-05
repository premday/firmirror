package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/daemon"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/vendors/dell"
	"github.com/premday/firmirror/pkg/vendors/hpe"
)

// refreshRun is what differs between a refresh command and a run of the daemon.
type refreshRun struct {
	overrides     Overrides
	alwaysPublish bool // Publish, and sign, even a document that did not change
	quietStop     bool // When told to end with nothing new, publish nothing: the daemon is stopping
	// inUse are the rings a run falls back to when the ones it is given would be
	// refused, as a bad commit must not stop the mirroring.
	inUse *bkc.File
	// beforePublish is called when the run is about to publish, after the signals
	// it was told to end on are dealt with.
	beforePublish func()
}

// refreshOutcome is what a refresh did besides publishing.
type refreshOutcome struct {
	newComponents int
	interrupted   bool      // The run was told to end, or timed out, before it processed every vendor
	vendorsFailed bool      // Some vendors failed to process
	holdsMetadata bool      // There is a document to publish: the stored one, or what the run mirrored
	published     bool      // The store holds the document built with rings
	fellBack      bool      // Other rings than the ones given were published, as those would be refused
	rings         *bkc.File // The rollout rings the run ended up with
}

// retryable is an error a new run may get past, which the message does not say.
type retryable struct{ error }

func (retryable) Is(target error) bool { return target == daemon.ErrRetryable }

var (
	errVendorsFailed = retryable{errors.New("one or more vendors failed to process")}
	errNotPublished  = retryable{errors.New("what this run mirrored was not published, as the store could not be checked or was published to meanwhile: it is mirrored again")}
	errInterrupted   = errors.New("ended before every vendor was processed")
)

// checkRefreshArgs fails on what a refresh cannot run without.
func checkRefreshArgs() error {
	if err := requireTools("fwupdtool", "jcat-tool"); err != nil {
		return err
	}
	if err := checkSigningKeys(); err != nil {
		return err
	}
	if args.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1, got %d", args.Concurrency)
	}
	if !args.HPEFlags.Enable && !args.DellFlags.Enable {
		return fmt.Errorf("no vendor enabled")
	}
	return nil
}

// runRefresh mirrors every enabled vendor and publishes the result.
func runRefresh(ctx context.Context, stop func()) error {
	if err := checkRefreshArgs(); err != nil {
		return err
	}
	// Before the storage is opened, which removes the temporaries of a killed write.
	definitions, err := loadDefinitions(ctx)
	if err != nil {
		return err
	}
	storage, err := openWritableStorage(ctx)
	if err != nil {
		return err
	}
	outcome, err := refresh(ctx, storage, definitions, refreshRun{
		overrides: args.Refresh.Overrides,
		beforePublish: func() {
			// Ending between the JCAT and the metadata leaves a JCAT that is not the
			// metadata's, which clients reject, so only a second signal ends the publish.
			// A signal that already ended the run counts as the first.
			endOnSecondSignal(ctx, func() {})
			stop()
		},
	})
	if outcome.vendorsFailed {
		err = errors.Join(err, errVendorsFailed)
	}
	if outcome.interrupted {
		// What was mirrored is published, but it is not all of a refresh: a Job that
		// ended this way must not count as one that succeeded.
		err = errors.Join(err, errInterrupted)
	}
	return err
}

// refresh mirrors every enabled vendor into the storage and publishes the result,
// tagged as the definitions say. It reads the stored metadata afresh, so it can run
// again.
func refresh(ctx context.Context, storage firmirror.Storage, definitions *bkc.File, run refreshRun) (outcome refreshOutcome, runErr error) {
	config := publishConfig(definitions, run.overrides)
	config.CacheDir = ".firmirror_cache"
	config.MaxConcurrency = args.Concurrency
	config.CleanupUnreferencedPackages = args.S3.Cleanup
	config.AlwaysPublish = run.alwaysPublish
	config.InUseBKC = run.inUse
	if args.BKC.File != "" {
		// A promotion committed during a long run must not be reverted by the
		// file the run started with.
		config.ReloadBKC = func(ctx context.Context) (*bkc.File, error) { return loadRings(ctx, args.BKC.File) }
	}

	fm, err := firmirror.NewFirmirrorSyncer(config, storage)
	if err != nil {
		return outcome, fmt.Errorf("failed to create syncer: %w", err)
	}

	if args.HPEFlags.Enable {
		for _, gen := range args.HPEFlags.Gens {
			hpeRepo := "fwpp-" + gen
			hpeVendor := hpe.NewHPEVendor(hpeRepo, args.HPEFlags.BaseURL)
			fm.RegisterVendor("hpe-"+gen, hpeVendor)
		}
	}

	if args.DellFlags.Enable {
		dellVendor := dell.NewDellVendor(args.DellFlags.MachinesID, args.DellFlags.BaseURL)
		fm.RegisterVendor("dell", dellVendor)
	}

	// Load existing metadata to avoid reprocessing
	if err := fm.LoadMetadata(ctx); err != nil {
		return outcome, fmt.Errorf("failed to load existing metadata: %w", err)
	}
	// Fail before downloading anything on what the final publish would refuse.
	if err := fm.Preflight(ctx); err != nil {
		return outcome, err
	}

	defer func() {
		// Before beforePublish, which ends the context. Not a timeout, which does not
		// ask to stop starting anything.
		told := errors.Is(ctx.Err(), context.Canceled)
		if run.beforePublish != nil {
			run.beforePublish()
		}
		// The daemon told to end with nothing new does not publish, and so does not
		// sign the same document for every pod stopped meanwhile, nor read the rings or
		// the bucket while it should be leaving. A refresh command still does, as it
		// would have: it repairs, reads the rings again and cleans up.
		if !(told && run.quietStop) || fm.GetNewComponentCount() > 0 {
			slog.Info("Saving repository metadata")
			if saveErr := fm.SaveMetadata(context.Background()); saveErr != nil {
				slog.Error("Failed to save metadata", "error", saveErr)
				runErr = errors.Join(runErr, fmt.Errorf("saving repository metadata: %w", saveErr))
			}
		} else {
			slog.Info("Told to end with nothing new, not publishing")
		}
		outcome.newComponents = fm.GetNewComponentCount()
		outcome.holdsMetadata = fm.HoldsMetadata()
		outcome.rings = fm.Config.BKC
		outcome.published = fm.Published()
		outcome.fellBack = fm.FellBack() && fm.Published()
	}()

	slog.Info("Starting firmware processing", "vendors", len(fm.GetAllVendors()))
	startTime := time.Now()

	for vendorName, vendor := range fm.GetAllVendors() {
		if ctx.Err() != nil {
			break
		}

		slog.Info("Processing vendor", "name", vendorName)
		// A vendor that stopped because it was told to end did not fail.
		if err := fm.ProcessVendor(ctx, vendor, vendorName); err != nil && ctx.Err() == nil {
			slog.Error("Failed to process vendor", "vendor", vendorName, "error", err)
			outcome.vendorsFailed = true
		}
	}

	// A context that ended, whatever the vendor it ended in, cut the run short.
	outcome.interrupted = ctx.Err() != nil
	slog.Info("Firmware processing completed",
		"duration", time.Since(startTime).Round(time.Second),
		"new_components", fm.GetNewComponentCount(),
		"vendors_processed", len(fm.GetAllVendors()))

	return outcome, nil
}
