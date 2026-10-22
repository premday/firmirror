package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/premday/firmirror/pkg/firmirror"
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

var args struct {
	DellFlags       `embed:"" prefix:"dell." group:"Dell" help:"Dell firmware fetching."`
	HPEFlags        `embed:"" prefix:"hpe." group:"HPE" help:"HPE firmware fetching."`
	S3              `embed:"" prefix:"s3." group:"S3 Storage" help:"S3 storage backend configuration."`
	Signature       `embed:"" prefix:"sign." group:"Signature" help:"Metadata signing configuration."`
	OutputDir       string `help:"Output directory for the LVFS-compatible firmware repository (ignored when using S3)" type:"path"`
	FirmwareBaseURL string `help:"Public URL the output directory or bucket is served from, as the fwupd remote's FirmwareBaseURI sees it. When set, the metadata names each package by absolute URL under it; without it the names are bare file names, which fwupd rejects with 'failed to parse url' for a remote that sets FirmwareBaseURI." default:""`
	Concurrency     int    `help:"Maximum number of firmware entries downloaded and processed concurrently per vendor" default:"8"`
	Refresh         struct {
		StopSigning bool `help:"Publish without signing keys over a JCAT that is signed, which clients that require signed metadata then reject: to stop signing on purpose. Without it such a refresh fails, before it mirrors anything." default:"false"`
	} `cmd:"" help:"Refresh all the firmware from the repositories. Note: this will not replace the already-existing firmware, even if the vendor pushed an updated version. You will need to delete the firmware manually."`
}

func main() {
	cli := kong.Parse(&args)

	if err := run(cli.Command()); err != nil {
		slog.Error("firmirror exited with error", "error", err)
		os.Exit(1)
	}
}

func run(command string) error {
	// Monitor for shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	switch command {
	case "refresh":
		return runRefresh(ctx, stop)
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
