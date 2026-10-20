package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/vendors/dell"
	"github.com/premday/firmirror/pkg/vendors/hpe"
)

// runRefresh mirrors every enabled vendor and publishes the result.
func runRefresh(ctx context.Context, stop func()) (runErr error) {
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

	storage, err := openStorage(ctx)
	if err != nil {
		return err
	}

	config := firmirror.FirmirrorConfig{
		CacheDir:                    ".firmirror_cache",
		Certificate:                 args.Signature.Certificate,
		PrivateKey:                  args.Signature.PrivateKey,
		MaxConcurrency:              args.Concurrency,
		CleanupUnreferencedPackages: args.S3.Cleanup,
		FirmwareBaseURL:             args.FirmwareBaseURL,
	}

	fm, err := firmirror.NewFirmirrorSyncer(config, storage)
	if err != nil {
		return fmt.Errorf("failed to create syncer: %w", err)
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

	defer func() {
		stop()
		slog.Info("Saving repository metadata")
		if saveErr := fm.SaveMetadata(context.Background()); saveErr != nil {
			slog.Error("Failed to save metadata", "error", saveErr)
			runErr = errors.Join(runErr, fmt.Errorf("saving repository metadata: %w", saveErr))
		}
	}()

	// Load existing metadata to avoid reprocessing
	if err := fm.LoadMetadata(ctx); err != nil {
		return fmt.Errorf("failed to load existing metadata: %w", err)
	}

	slog.Info("Starting firmware processing", "vendors", len(fm.GetAllVendors()))
	startTime := time.Now()

	var hasError bool
	for vendorName, vendor := range fm.GetAllVendors() {
		if ctx.Err() != nil {
			break
		}

		slog.Info("Processing vendor", "name", vendorName)
		if err := fm.ProcessVendor(ctx, vendor, vendorName); err != nil && err != context.Canceled {
			slog.Error("Failed to process vendor", "vendor", vendorName, "error", err)
			hasError = true
		}
	}

	slog.Info("Firmware processing completed",
		"duration", time.Since(startTime).Round(time.Second),
		"new_components", fm.GetNewComponentCount(),
		"vendors_processed", len(fm.GetAllVendors()))

	if hasError {
		return fmt.Errorf("one or more vendors failed to process")
	}
	return nil
}
