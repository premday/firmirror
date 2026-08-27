package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/vendors/dell"
	"github.com/premday/firmirror/pkg/vendors/hpe"
)

type DellFlags struct {
	Enable     bool     `help:"Enable Dell firmware fetching." default:"false"`
	MachinesID []string `help:"List of machine IDs to fetch firmware for. They are composed of 4 characters in hexadecimal representing the machine type. For example: \"0C60\" for \"3168\" corresponding to the C6615 series of servers. You can also specify \"*\" to fetch all the firmware, but this may take a very long time."`
}

type HPEFlags struct {
	Enable bool     `help:"Enable HPE firmware fetching." default:"false"`
	Gens   []string `help:"List of generations to fetch firmware for." default:"gen8,gen9,gen10,gen11,gen12" enum:"gen8,gen9,gen10,gen11,gen12"`
}

type S3 struct {
	Enable   bool   `help:"Use S3 storage backend instead of local filesystem. Requires AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables" default:"false"`
	Cleanup  bool   `help:"Replace rebuilt releases in metadata and delete unreferenced CAB packages from S3 after saving metadata." default:"false"`
	Bucket   string `help:"S3 bucket name for storing firmware files"`
	Prefix   string `help:"Optional prefix for all S3 keys" default:""`
	Region   string `help:"AWS region" default:"us-east-1"`
	Endpoint string `help:"Custom S3 endpoint URL (for S3-compatible services like MinIO)" default:""`
}

type Signature struct {
	Certificate string `help:"Path to certificate file for signing metadata (.pem or .crt)" type:"path"`
	PrivateKey  string `help:"Path to private key file for signing metadata (.pem or .key)" type:"path"`
}

var args struct {
	DellFlags   `embed:"" prefix:"dell." group:"Dell" help:"Dell firmware fetching."`
	HPEFlags    `embed:"" prefix:"hpe." group:"HPE" help:"HPE firmware fetching."`
	S3          `embed:"" prefix:"s3." group:"S3 Storage" help:"S3 storage backend configuration."`
	Signature   `embed:"" prefix:"sign." group:"Signature" help:"Metadata signing configuration."`
	OutputDir   string `help:"Output directory for the LVFS-compatible firmware repository (ignored when using S3)" type:"path"`
	Concurrency int    `help:"Maximum number of firmware entries downloaded and processed concurrently per vendor" default:"8"`
	Refresh     struct {
	} `cmd:"" help:"Refresh all the firmware from the repositories. Note: this will not replace the already-existing firmware, even if the vendor pushed an updated version. You will need to delete the firmware manually."`
}

func main() {
	cli := kong.Parse(&args)
	switch cli.Command() {
	case "refresh":
	default:
		panic(cli.Command())
	}

	if err := run(); err != nil {
		slog.Error("firmirror exited with error", "error", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	// Check if bin tools are available
	for _, bin := range []string{"fwupdtool", "jcat-tool"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s is required but not found in PATH", bin)
		}
	}

	// Monitor for shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	certProvided := args.Signature.Certificate != ""
	keyProvided := args.Signature.PrivateKey != ""
	if certProvided && keyProvided {
		if _, err := os.Stat(args.Signature.Certificate); err != nil {
			return fmt.Errorf("certificate file not accessible: %s: %w", args.Signature.Certificate, err)
		}
		if _, err := os.Stat(args.Signature.PrivateKey); err != nil {
			return fmt.Errorf("private key file not accessible: %s: %w", args.Signature.PrivateKey, err)
		}
	} else if certProvided || keyProvided {
		return fmt.Errorf("both --sign.certificate and --sign.private-key must be provided together, or neither")
	} else {
		slog.Warn("No certificate or private key provided, metadata will not be signed")
	}

	var storage firmirror.Storage
	var err error

	if args.S3.Enable {
		storage, err = firmirror.NewS3Storage(ctx, args.S3.Bucket, args.S3.Prefix, args.S3.Region, args.S3.Endpoint)
		if err != nil {
			return fmt.Errorf("failed to create S3 storage backend: %w", err)
		}
		slog.Info("Using S3 storage backend", "bucket", args.S3.Bucket, "prefix", args.S3.Prefix)
	} else {
		if args.OutputDir == "" {
			return fmt.Errorf("output directory is required when using local storage")
		}

		storage, err = firmirror.NewLocalStorage(args.OutputDir)
		if err != nil {
			return fmt.Errorf("failed to create local storage backend: %w", err)
		}
		slog.Info("Using local filesystem storage", "path", args.OutputDir)
	}

	if args.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1, got %d", args.Concurrency)
	}

	if !args.HPEFlags.Enable && !args.DellFlags.Enable {
		return fmt.Errorf("no vendor enabled")
	}

	config := firmirror.FirmirrorConfig{
		CacheDir:                    ".firmirror_cache",
		Certificate:                 args.Signature.Certificate,
		PrivateKey:                  args.Signature.PrivateKey,
		MaxConcurrency:              args.Concurrency,
		CleanupUnreferencedPackages: args.S3.Cleanup,
	}

	fm, err := firmirror.NewFirmirrorSyncer(config, storage)
	if err != nil {
		return fmt.Errorf("failed to create syncer: %w", err)
	}

	if args.HPEFlags.Enable {
		for _, gen := range args.HPEFlags.Gens {
			hpeRepo := "fwpp-" + gen
			hpeVendor := hpe.NewHPEVendor(hpeRepo)
			fm.RegisterVendor("hpe-"+gen, hpeVendor)
		}
	}

	if args.DellFlags.Enable {
		dellVendor := dell.NewDellVendor(args.DellFlags.MachinesID)
		fm.RegisterVendor("dell", dellVendor)
	}

	defer func() {
		stop()
		slog.Info("Saving repository metadata")
		if saveErr := fm.SaveMetadata(context.Background()); saveErr != nil {
			slog.Error("Failed to save metadata", "error", saveErr)
			if args.S3.Cleanup {
				runErr = errors.Join(runErr, fmt.Errorf("saving repository metadata: %w", saveErr))
			}
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
