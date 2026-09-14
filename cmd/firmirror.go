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
	Bucket   string `help:"S3 bucket name for storing firmware files"`
	Prefix   string `help:"Optional prefix for all S3 keys" default:""`
	Region   string `help:"AWS region" default:"us-east-1"`
	Endpoint string `help:"Custom S3 endpoint URL (for S3-compatible services like MinIO)" default:""`
}

type S3CleanupCmd struct {
	MinPackageAge time.Duration `help:"Keep a package that nothing references until it is at least this old. A refresh publishes the metadata naming its packages only when the run ends, so a shorter window risks deleting what a run in progress is about to publish. Set it to 0 to reclaim regardless of age." default:"24h"`
}

type Signature struct {
	Certificate string `help:"Path to certificate file for signing metadata (.pem or .crt)" type:"path"`
	PrivateKey  string `help:"Path to private key file for signing metadata (.pem or .key)" type:"path"`
}

type PromoteCmd struct {
	To   string `help:"Ring to promote into. Its snapshot is replaced by the source one, and its feed is published as metadata-<ring>.xml.zst." required:""`
	From string `help:"Ring to promote from. Defaults to the index, which holds the latest mirrored firmware." default:""`
}

var args struct {
	DellFlags   `embed:"" prefix:"dell." group:"Dell" help:"Dell firmware fetching."`
	HPEFlags    `embed:"" prefix:"hpe." group:"HPE" help:"HPE firmware fetching."`
	S3          `embed:"" prefix:"s3." group:"S3 Storage" help:"S3 storage backend configuration."`
	Signature   `embed:"" prefix:"sign." group:"Signature" help:"Metadata signing configuration."`
	OutputDir   string   `help:"Output directory for the LVFS-compatible firmware repository (ignored when using S3)" type:"path"`
	Concurrency int      `help:"Maximum number of firmware entries downloaded and processed concurrently per vendor" default:"8"`
	Block       []string `help:"Firmware withheld from the published ring feeds, as either the vendor firmware filename or the CAB name. Can be specified multiple times. Blocked firmware stays mirrored, so removing an entry publishes it again without downloading anything." name:"block"`
	NoLock      bool     `help:"Run without taking the repository lock. The lock relies on conditional writes, which some S3-compatible endpoints do not implement; without it nothing prevents a second firmirror from writing the same repository at the same time." name:"no-lock" default:"false"`
	Refresh     struct {
	} `cmd:"" help:"Refresh all the firmware from the repositories. Note: this will not replace the already-existing firmware, even if the vendor pushed an updated version. You will need to delete the firmware manually."`
	S3Cleanup S3CleanupCmd `cmd:"" name:"s3-cleanup" help:"Replace the releases a vendor rebuild superseded in the index, then delete the stored firmware packages that nothing references. Everything the index, a ring snapshot or a ring feed still points at is kept, so a package only an older ring serves is never removed."`
	Publish   struct {
	} `cmd:"" help:"Republish the feed of every ring from its snapshot, without promoting anything. Run it to apply a blocklist change straight away instead of waiting for the next refresh."`
	Promote PromoteCmd `cmd:"" help:"Promote one ring into another, so hosts on the target ring get the firmware the source ring was serving. The source state is snapshotted, so a later promotion out of the target ring publishes what was validated on it rather than whatever has been mirrored since."`
}

func main() {
	cli := kong.Parse(&args)

	var err error
	switch cli.Command() {
	case "refresh":
		err = runRefresh()
	case "promote":
		err = runPromote()
	case "publish":
		err = runPublish()
	case "s3-cleanup":
		err = runS3Cleanup()
	default:
		panic(cli.Command())
	}

	if err != nil {
		slog.Error("firmirror exited with error", "error", err)
		os.Exit(1)
	}
}

func requireTools(bins ...string) error {
	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s is required but not found in PATH", bin)
		}
	}
	return nil
}

// lockRepository takes the lock that serializes everything writing to the
// repository, unless the operator opted out of it.
func lockRepository(ctx context.Context, fm *firmirror.FirmirrorSyncer) (*firmirror.RepositoryLock, error) {
	if args.NoLock {
		slog.Warn("Running without the repository lock, nothing prevents a concurrent firmirror from writing the same documents")
		return firmirror.UnlockedRepository(ctx), nil
	}
	return fm.LockRepository(ctx)
}

func releaseRepositoryLock(lock *firmirror.RepositoryLock, runErr *error) {
	if err := lock.Release(context.Background()); err != nil {
		*runErr = errors.Join(*runErr, err)
	}
}

// newSyncer performs the setup every subcommand needs: validating the signing
// material, opening the storage backend and creating the syncer.
func newSyncer(ctx context.Context) (*firmirror.FirmirrorSyncer, error) {
	certProvided := args.Signature.Certificate != ""
	keyProvided := args.Signature.PrivateKey != ""
	if certProvided && keyProvided {
		if _, err := os.Stat(args.Signature.Certificate); err != nil {
			return nil, fmt.Errorf("certificate file not accessible: %s: %w", args.Signature.Certificate, err)
		}
		if _, err := os.Stat(args.Signature.PrivateKey); err != nil {
			return nil, fmt.Errorf("private key file not accessible: %s: %w", args.Signature.PrivateKey, err)
		}
	} else if certProvided || keyProvided {
		return nil, fmt.Errorf("both --sign.certificate and --sign.private-key must be provided together, or neither")
	} else {
		slog.Warn("No certificate or private key provided, metadata will not be signed")
	}

	var storage firmirror.Storage
	var err error

	if args.S3.Enable {
		storage, err = firmirror.NewS3Storage(ctx, args.S3.Bucket, args.S3.Prefix, args.S3.Region, args.S3.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to create S3 storage backend: %w", err)
		}
		slog.Info("Using S3 storage backend", "bucket", args.S3.Bucket, "prefix", args.S3.Prefix)
	} else {
		if args.OutputDir == "" {
			return nil, fmt.Errorf("output directory is required when using local storage")
		}

		storage, err = firmirror.NewLocalStorage(args.OutputDir)
		if err != nil {
			return nil, fmt.Errorf("failed to create local storage backend: %w", err)
		}
		slog.Info("Using local filesystem storage", "path", args.OutputDir)
	}

	config := firmirror.FirmirrorConfig{
		CacheDir:       ".firmirror_cache",
		Certificate:    args.Signature.Certificate,
		PrivateKey:     args.Signature.PrivateKey,
		MaxConcurrency: args.Concurrency,
		Blocklist:      args.Block,
		MinPackageAge:  args.S3Cleanup.MinPackageAge,
	}

	fm, err := firmirror.NewFirmirrorSyncer(config, storage)
	if err != nil {
		return nil, fmt.Errorf("failed to create syncer: %w", err)
	}
	return fm, nil
}

// runPromote publishes an existing state to another ring. No firmware is
// downloaded, so only the metadata signing tool is needed.
func runPromote() (runErr error) {
	if err := requireTools("jcat-tool"); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	fm, err := newSyncer(ctx)
	if err != nil {
		return err
	}
	lock, err := lockRepository(ctx, fm)
	if err != nil {
		return err
	}
	defer releaseRepositoryLock(lock, &runErr)

	return fm.Promote(lock.Work, args.Promote.From, args.Promote.To)
}

// runPublish applies the current blocklist to every ring feed. Nothing is
// downloaded and no ring changes the state it serves.
func runPublish() (runErr error) {
	if err := requireTools("jcat-tool"); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	fm, err := newSyncer(ctx)
	if err != nil {
		return err
	}
	lock, err := lockRepository(ctx, fm)
	if err != nil {
		return err
	}
	defer releaseRepositoryLock(lock, &runErr)

	_, err = fm.PublishFeeds(lock.Work)
	return err
}

// runS3Cleanup reclaims what mirroring firmware deliberately leaves behind.
// Split out of refresh on purpose: a run whose job is to mirror firmware
// should not drop a release or delete a package as a side effect, and one that
// failed part-way leaves an index that does not yet list everything it will.
func runS3Cleanup() (runErr error) {
	if err := requireTools("jcat-tool"); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	fm, err := newSyncer(ctx)
	if err != nil {
		return err
	}
	lock, err := lockRepository(ctx, fm)
	if err != nil {
		return err
	}
	defer releaseRepositoryLock(lock, &runErr)

	return fm.CleanupPackages(lock.Work)
}

func runRefresh() (runErr error) {
	// Check if bin tools are available
	if err := requireTools("fwupdtool", "jcat-tool"); err != nil {
		return err
	}

	// Monitor for shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	if args.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1, got %d", args.Concurrency)
	}

	if !args.HPEFlags.Enable && !args.DellFlags.Enable {
		return fmt.Errorf("no vendor enabled")
	}

	fm, err := newSyncer(ctx)
	if err != nil {
		return err
	}
	lock, err := lockRepository(ctx, fm)
	if err != nil {
		return err
	}
	defer releaseRepositoryLock(lock, &runErr)
	ctx = lock.Work

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
		// The lock context rather than a fresh one: the metadata this run
		// owes its repository has to be committed after the signal that
		// ended the run, but not after the lock protecting it was lost.
		if saveErr := fm.SaveMetadata(lock.Held); saveErr != nil {
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
