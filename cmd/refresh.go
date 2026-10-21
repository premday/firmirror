package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/vendors/dell"
	"github.com/premday/firmirror/pkg/vendors/hpe"
)

// refreshRun is what differs between a refresh command and a run of the daemon.
type refreshRun struct {
	overrides     Overrides
	alwaysPublish bool // Publish, and sign, even a document that did not change
	// beforePublish is called when the run is about to publish, after the signals
	// it was told to end on are dealt with.
	beforePublish func()
}

// refreshOutcome is what a refresh did besides publishing.
type refreshOutcome struct {
	newComponents int
	vendorsFailed bool // Some vendors failed to process, and what was mirrored was published
}

var errVendorsFailed = errors.New("one or more vendors failed to process")

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
	storage, err := openWritableStorage(ctx)
	if err != nil {
		return err
	}
	definitions, err := loadDefinitions(ctx)
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
		if run.beforePublish != nil {
			run.beforePublish()
		}
		slog.Info("Saving repository metadata")
		if saveErr := fm.SaveMetadata(context.Background()); saveErr != nil {
			slog.Error("Failed to save metadata", "error", saveErr)
			runErr = errors.Join(runErr, fmt.Errorf("saving repository metadata: %w", saveErr))
		}
		outcome.newComponents = fm.GetNewComponentCount()
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

	slog.Info("Firmware processing completed",
		"duration", time.Since(startTime).Round(time.Second),
		"new_components", fm.GetNewComponentCount(),
		"vendors_processed", len(fm.GetAllVendors()))

	return outcome, nil
}
