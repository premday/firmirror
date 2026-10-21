package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/premday/firmirror/pkg/bkc"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/utils"
)

type DellFlags struct {
	Enable     bool     `help:"Enable Dell firmware fetching." default:"false"`
	MachinesID []string `help:"List of machine IDs to fetch firmware for. They are composed of 4 characters in hexadecimal representing the machine type. For example: \"0C60\" for \"3168\" corresponding to the C6615 series of servers. You can also specify \"*\" to fetch all the firmware, but this may take a very long time."`
	BaseURL    string   `help:"Read the catalog and the firmware from this copy of https://dl.dell.com instead of the site itself. It takes an HTTP(S) URL, a file:// URL, or a plain directory, so an rsync mirror can be read where it sits. The published metadata is unchanged: it points at the location the catalog itself names." default:"https://dl.dell.com"`
}

type HPEFlags struct {
	Enable  bool     `help:"Enable HPE firmware fetching." default:"false"`
	Gens    []string `help:"List of generations to fetch firmware for." default:"gen8,gen9,gen10,gen11,gen12" enum:"gen8,gen9,gen10,gen11,gen12"`
	BaseURL string   `help:"Read the firmware from this copy of https://downloads.linux.hpe.com/SDR/repo instead of the SDR repository itself. It takes an HTTP(S) URL, a file:// URL, or a plain directory, so an rsync mirror can be read where it sits. It is the directory holding the fwpp-<gen> repositories, and the published metadata keeps pointing at the public one." default:"https://downloads.linux.hpe.com/SDR/repo"`
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

type BKC struct {
	File      string   `help:"What each rollout ring serves: a YAML file, or the HTTP(S) URL it is fetched from. A refresh tags the releases it publishes accordingly, and publishes no ring tag without it. bkc promote edits it, so it is a path there." default:""`
	AllowHTTP bool     `help:"Read the rollout rings, and the --metadata of bkc promote, from a plain HTTP URL too, which whoever is on the way can change, and which decides what every host is served. Only for a network that is trusted." default:"false"`
	Rings     []string `help:"The rings of a file bkc promote creates, in promotion order: the first takes the newest mirrored release when it is promoted, and each other ring is promoted from the one before it. A file that exists keeps its own. Default: ${rings}."`
}

var args struct {
	DellFlags       `embed:"" prefix:"dell." group:"Dell" help:"Dell firmware fetching."`
	HPEFlags        `embed:"" prefix:"hpe." group:"HPE" help:"HPE firmware fetching."`
	S3              `embed:"" prefix:"s3." group:"S3 Storage" help:"S3 storage backend configuration."`
	Signature       `embed:"" prefix:"sign." group:"Signature" help:"Metadata signing configuration."`
	BKC             `embed:"" prefix:"bkc." group:"Rollout rings" help:"Rollout ring configuration."`
	OutputDir       string `help:"Output directory for the LVFS-compatible firmware repository (ignored when using S3)" type:"path"`
	FirmwareBaseURL string `help:"Public URL the output directory or bucket is served from, as the fwupd remote's FirmwareBaseURI sees it. When set, the metadata names each package by absolute URL under it; without it the names are bare file names, which fwupd rejects with 'failed to parse url' for a remote that sets FirmwareBaseURI." default:""`
	Concurrency     int    `help:"Maximum number of firmware entries downloaded and processed concurrently per vendor" default:"8"`
	Refresh         struct {
		Force       bool `help:"Publish even if that stops publishing a ring tag the stored metadata has, as an absent or stale --bkc.file, another tag prefix or a removed ring would, which takes the hosts on that ring off it. Without it such a refresh fails, before it mirrors anything where it can." default:"false"`
		StopSigning bool `help:"Publish without signing keys over a JCAT that is signed, which clients that require signed metadata then reject: to stop signing on purpose. Without it such a refresh fails, before it mirrors anything." default:"false"`
		DropBaseURL bool `help:"Publish package locations without the base URL the stored metadata has, as a forgotten --firmware-base-url would, which fwupd remotes that set FirmwareBaseURI reject: to drop it on purpose. Without it such a refresh fails, before it mirrors anything." default:"false"`
	} `cmd:"" help:"Refresh all the firmware from the repositories. Note: this will not replace the already-existing firmware, even if the vendor pushed an updated version. You will need to delete the firmware manually."`

	Rollout struct {
		Status struct {
			Ring string `help:"Only report this ring." default:""`
		} `cmd:"" help:"Report which version each rollout ring serves for each component, from --bkc.file."`

		Promote struct {
			Ring           string   `arg:"" help:"The ring to promote into. The first ring takes the newest release of every mirrored component, any other ring what the ring before it serves."`
			Component      []string `help:"Only promote these component IDs. Can be repeated. Omit to promote every component."`
			Metadata       string   `help:"Read the mirrored components from this published metadata, a path or an HTTP(S) URL, instead of from the storage." default:""`
			DryRun         bool     `help:"Report what would change without writing the file." default:"false"`
			AllowDowngrade bool     `help:"Allow a promotion that moves the ring backwards. fwupdmgr sync installs older releases too, so this downgrades the hosts on the ring." default:"false"`
			Prune          bool     `help:"Also remove the entries of components the mirror no longer has. Not with --component." default:"false"`
			AllowMassPrune bool     `help:"Let --prune remove more than half of the entries of the file, which a mirror that is partial or of another environment would." default:"false"`
		} `cmd:"" help:"Move a ring on, in the file --bkc.file names, which is created if it does not exist. Nothing is published: the file is committed, and the next refresh or regen-metadata publishes it."`
	} `cmd:"" name:"bkc" help:"Inspect and promote the rollout rings."`
}

func main() {
	cli := kong.Parse(&args, parserVars)

	if err := run(cli.Command()); err != nil {
		slog.Error("firmirror exited with error", "error", err)
		os.Exit(1)
	}
}

// parserVars are the values the help tags of args refer to.
var parserVars = kong.Vars{"rings": strings.Join(bkc.DefaultRings, ",")}

func run(command string) error {
	// Monitor for shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	switch command {
	case "refresh":
		return runRefresh(ctx, stop)
	case "bkc status":
		return runBKCStatus(ctx)
	case "bkc promote <ring>":
		return runBKCPromote(ctx)
	default:
		panic(command)
	}
}

// requireTools checks for the external tools a command actually uses, so a
// command that builds or signs nothing runs on a machine without them.
func requireTools(tools ...string) error {
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is required but not found in PATH", tool)
		}
	}
	return nil
}

// checkSigningKeys accepts a certificate and a key together, or neither.
func checkSigningKeys() error {
	certProvided := args.Signature.Certificate != ""
	keyProvided := args.Signature.PrivateKey != ""

	switch {
	case certProvided && keyProvided:
		if _, err := os.Stat(args.Signature.Certificate); err != nil {
			return fmt.Errorf("certificate file not accessible: %s: %w", args.Signature.Certificate, err)
		}
		if _, err := os.Stat(args.Signature.PrivateKey); err != nil {
			return fmt.Errorf("private key file not accessible: %s: %w", args.Signature.PrivateKey, err)
		}
	case certProvided || keyProvided:
		return fmt.Errorf("both --sign.certificate and --sign.private-key must be provided together, or neither")
	default:
		slog.Warn("No certificate or private key provided, metadata will not be signed")
	}

	return nil
}

// openStorage builds the storage backend the flags describe.
func openStorage(ctx context.Context) (firmirror.Storage, error) {
	if args.S3.Enable {
		storage, err := firmirror.NewS3Storage(ctx, args.S3.Bucket, args.S3.Prefix, args.S3.Region, args.S3.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to create S3 storage backend: %w", err)
		}
		slog.Info("Using S3 storage backend", "bucket", args.S3.Bucket, "prefix", args.S3.Prefix)
		return storage, nil
	}

	if args.OutputDir == "" {
		return nil, fmt.Errorf("output directory is required when using local storage")
	}
	storage, err := firmirror.NewLocalStorage(args.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create local storage backend: %w", err)
	}
	slog.Info("Using local filesystem storage", "path", args.OutputDir)
	return storage, nil
}

// checkOutputDir fails when the output directory does not exist, so that a
// command that only reads does not create it.
func checkOutputDir() error {
	if !args.S3.Enable && args.OutputDir != "" {
		if _, err := os.Stat(args.OutputDir); err != nil {
			return fmt.Errorf("output directory: %w", err)
		}
	}
	return nil
}

// openSyncer is a syncer on the configured storage with the stored metadata
// loaded, for a command that mirrors nothing. It uses a temporary cache directory,
// removed by the returned function, so it does not touch the work directories of a
// refresh running from the same directory.
func openSyncer(ctx context.Context, config firmirror.FirmirrorConfig) (*firmirror.FirmirrorSyncer, func(), error) {
	if err := checkOutputDir(); err != nil {
		return nil, nil, err
	}
	storage, err := openStorage(ctx)
	if err != nil {
		return nil, nil, err
	}
	cacheDir, err := os.MkdirTemp("", "firmirror-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(cacheDir) }
	config.CacheDir = cacheDir
	syncer, err := firmirror.NewFirmirrorSyncer(config, storage)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create syncer: %w", err)
	}
	if err := syncer.LoadMetadata(ctx); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to load existing metadata: %w", err)
	}
	return syncer, cleanup, nil
}

// loadDefinitions reads the rollout ring definitions, or returns nil when no
// file is given.
func loadDefinitions(ctx context.Context) (*bkc.File, error) {
	if args.BKC.File == "" {
		return nil, nil
	}
	return loadRings(ctx, args.BKC.File)
}

// loadRings reads the definitions file, hinting at --bkc.allow-http when plain
// HTTP is what refused it.
func loadRings(ctx context.Context, source string) (*bkc.File, error) {
	file, err := bkc.Load(ctx, source, args.BKC.AllowHTTP)
	return file, allowHTTPHint(err)
}

func allowHTTPHint(err error) error {
	if errors.Is(err, utils.ErrPlainHTTP) {
		return fmt.Errorf("%w (--bkc.allow-http allows it)", err)
	}
	return err
}
