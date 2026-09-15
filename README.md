# Firmirror

A firmware mirroring tool that creates [LVFS](https://fwupd.org/)-compatible repositories by fetching and converting firmware from hardware vendors (Dell and HPE).

## Overview

Firmirror automates the process of:

- Fetching firmware catalogs from vendor sources (Dell DSU, HPE SDR)
- Downloading firmware packages
- Converting vendor-specific formats to LVFS/fwupd AppStream metadata
- Building CAB packages compatible with fwupd
- Maintaining an LVFS-compatible metadata index with incremental updates
- Signing metadata using JCAT format with X.509 certificates

This allows you to host your own firmware mirror that `fwupd` clients can consume, without depending on the vendor's LVFS remote.

## Features

- **Multi-vendor Support**: Dell (DSU catalog) and HPE (SDR repositories)
- **Incremental Processing**: Tracks already-processed firmware to avoid re-downloading and re-processing on subsequent runs
- **Explicit Cleanup**: a separate command resolves rebuilt packages and deletes only what no metadata document references
- **Concurrent Downloads**: Configurable concurrency for downloading and processing firmware entries
- **Pluggable Storage**: Local filesystem or S3 (including S3-compatible services like MinIO)
- **Metadata Signing**: JCAT signatures with SHA256 checksums and optional PKCS#7 X.509 signatures
- **Progressive Rollout**: per-ring metadata documents, promoted explicitly, with a blocklist to withhold a bad firmware
- **Helm Chart**: Kubernetes CronJob deployment with S3/local storage support

## Prerequisites

- Go 1.27+
- `fwupdtool` (for building CAB packages)
- `jcat-tool` (for creating JCAT signature files)

On Debian/Ubuntu:

```bash
sudo apt install fwupd jcat
```

## Installation

### From Source

```bash
go build -o firmirror ./cmd/firmirror.go
```

### Docker

A multi-stage Dockerfile is included. The resulting image contains the `firmirror` binary, `fwupdtool`, and `jcat-tool`.

```bash
docker build -t firmirror .
```

Pre-built images are published to `ghcr.io/premday/firmirror` via CI.

## Usage

### Subcommands

| Command | What it does |
|---------|--------------|
| `refresh` | Mirror new firmware from the vendors into the index, then republish the ring feeds |
| `promote --to=<ring> [--from=<ring>]` | Publish the state of one ring to another. See [Rings](#rings) |
| `publish` | Republish every ring feed from its snapshot, applying the current blocklist |
| `s3-cleanup` | Replace the releases a vendor rebuild superseded, then delete the packages nothing references |

### Basic Commands

```bash
# Mirror Dell firmware for specific machine types
./firmirror refresh /output/dir \
  --dell.enable \
  --dell.machines-id=0C60 \
  --dell.machines-id=0C61

# Mirror Dell firmware (all, no filter)
./firmirror refresh /output/dir \
  --dell.enable

# Mirror HPE firmware for specific generations
./firmirror refresh /output/dir \
  --hpe.enable \
  --hpe.gens=gen10,gen11

# Mirror both vendors
./firmirror refresh /output/dir \
  --dell.enable \
  --dell.machines-id=0C60 \
  --hpe.enable \
  --hpe.gens=gen10

# With metadata signing
./firmirror refresh /output/dir \
  --hpe.enable \
  --hpe.gens=gen10,gen11,gen12 \
  --sign.certificate=cert.pem \
  --sign.private-key=key.pem

# Using S3 storage instead of local filesystem
./firmirror refresh \
  --s3.enable \
  --s3.bucket=my-firmware-bucket \
  --s3.prefix=firmirror \
  --s3.region=us-east-1 \
  --hpe.enable \
  --hpe.gens=gen11
```

### CLI Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--output-dir` | Output directory for the LVFS repository (local storage only) | (required) |
| `--concurrency` | Max concurrent firmware downloads per vendor | `8` |
| `--no-lock` | Run without the repository lock, for an S3 endpoint that does not implement conditional writes | `false` |
| **Dell** | | |
| `--dell.enable` | Enable Dell firmware mirroring | `false` |
| `--dell.machines-id` | Machine System IDs to filter (4-char hex, e.g. `0C60`). Can be specified multiple times. Omit to include all firmware | (optional) |
| **HPE** | | |
| `--hpe.enable` | Enable HPE firmware mirroring | `false` |
| `--hpe.gens` | Generations to fetch (`gen8`–`gen12`) | `gen8,gen9,gen10,gen11,gen12` |
| `--block` | Firmware withheld from the published ring feeds, as either the vendor firmware filename or the CAB name. Can be specified multiple times | (optional) |
| `--min-package-age` | On `s3-cleanup`: keep a package nothing references until it is at least this old | `24h` |
| **S3 Storage** | | |
| `--s3.enable` | Use S3 storage backend instead of local filesystem | `false` |
| `--s3.bucket` | S3 bucket name | (required if S3 enabled) |
| `--s3.prefix` | Optional prefix for all S3 keys | `""` |
| `--s3.region` | AWS region | `us-east-1` |
| `--s3.endpoint` | Custom S3 endpoint URL (for MinIO, etc.) | `""` |
| **Signing** | | |
| `--sign.certificate` | Path to X.509 certificate (.pem or .crt) | `""` |
| `--sign.private-key` | Path to private key (.pem or .key) | `""` |

### Output Structure

```
/output/dir/
├── <sha256>-firmware1.bin.cab    # CAB packages (prefixed with SHA256 hash)
├── <sha256>-firmware2.bin.cab
├── snapshot.xml.zst              # Internal: the index, every firmware mirrored
├── metadata.xml.zst              # Compressed LVFS metadata published from it
├── metadata.xml.zst.jcat         # JCAT signature/checksum file
├── snapshot-<ring>.xml.zst       # Internal: the state a ring was promoted with
├── metadata-<ring>.xml.zst       # Compressed LVFS metadata served to that ring
└── metadata-<ring>.xml.zst.jcat
```

The `metadata.xml.zst` file contains the AppStream component metadata that `fwupd` uses to discover available firmware. The `.jcat` file provides integrity verification and optional signatures.

The `snapshot-` and `metadata-<ring>` documents only exist once a ring has been promoted into. See [Rings](#rings).

### Incremental Updates

Firmirror tracks which firmware files have already been processed in the metadata index. On subsequent runs, already-existing firmware is skipped, so only new entries are downloaded and processed.

To force re-processing of a firmware, delete its CAB file and remove it from the metadata.

## Rings

`fwupd` has no notion of a release channel: a client installs whatever the one
metadata document its remote points at lists. A ring is therefore its own
metadata document, published next to the cabinets so the relative `<location>`
of every release keeps resolving against the same packages, whichever ring a
host is on.

Every ring is two documents: the unfiltered state it holds, which is internal
to firmirror, and the feed published from it, which is what its hosts download.
The index is one of those rings, so the blocklist reaches the hosts reading it
too — the ones already running whatever firmware just turned out to be bad.

| Object | What it is | Written by |
|--------|------------|------------|
| `snapshot.xml.zst` | The index: every firmware mirrored so far, and the state promotions are made from. Internal, unsigned: clients never fetch it | `refresh` |
| `metadata.xml.zst` | The index feed: the index minus the blocklisted releases, for hosts that want the latest firmware the day it lands | `refresh` and `publish` |
| `snapshot-<ring>.xml.zst` | The state a ring was promoted with. Internal, unsigned: clients never fetch it | `promote` |
| `metadata-<ring>.xml.zst` | What hosts on the ring download: the snapshot minus the blocklisted releases | `promote`, `publish`, and `refresh` at the end of each run |

Ring names are free-form, and firmirror imposes no order between them: which
ring is promoted from which is decided by whoever runs `promote`.

A repository written by an earlier version has no `snapshot.xml.zst`: the first
`refresh` or `publish` records what `metadata.xml.zst` holds as the index and
publishes the feed from it, so nothing is mirrored again.

### Promoting

```bash
# Publish today's index to the beta ring
./firmirror promote --to=beta --s3.enable --s3.bucket=my-bucket

# Once beta has run it for a while, publish what beta is serving to preview.
# preview gets the state beta was validated on, not whatever has been mirrored
# since.
./firmirror promote --to=preview --from=beta --s3.enable --s3.bucket=my-bucket
```

A ring keeps serving its state until it is promoted into again, so hosts only
move when someone decides they should. In Kubernetes each promotion is a
suspended CronJob; `contrib/promote.sh <ring> <kube-context>` runs one.
Add `[namespace] [helm-release]` when either differs from `firmirror`.

### Withholding a bad firmware

`--block` withholds firmware from every ring feed, matching either the vendor
firmware filename or the CAB name that `fwupdmgr get-releases` reports as the
release location:

```bash
./firmirror refresh /output/dir --dell.enable --block=BIOS_ABC12_LN_1.2.3.BIN
```

Blocked firmware stays mirrored and stays in the index and the ring snapshots,
so removing the entry publishes it again without downloading anything. Only
what is published is filtered, the index feed included. The blocklist is
applied on every `refresh`, `promote` and `publish`; run `publish` to apply a
change immediately without promoting anything:

```bash
./firmirror publish --s3.enable --s3.bucket=my-bucket
```

Every cabinet that any of these documents references is kept in storage, so a
package only an older ring still points at survives [cleanup](#cleaning-up).

## Storage Backends

### Local Filesystem

The default backend. Write CAB packages and metadata directly to a directory on disk.

```bash
./firmirror refresh /data/firmware --hpe.enable --hpe.gens=gen11
```

### S3

Store firmware and metadata in an S3 bucket. Supports AWS S3 and S3-compatible services (MinIO, etc.) via the `--s3.endpoint` flag.

Requires `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` environment variables.

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...

./firmirror refresh \
  --s3.enable \
  --s3.bucket=my-bucket \
  --s3.prefix=firmirror \
  --s3.region=eu-west-1 \
  --hpe.enable --hpe.gens=gen11
```

### Cleaning up

`refresh` neither drops a release nor deletes a package. When a vendor rebuilds
a package, the new release is appended next to the one it replaces and both
stay in the index, because dropping one there would leave its `.cab` referenced
by nothing on a run whose job is only to mirror firmware. Each run reports what
a cleanup would reclaim.

```bash
./firmirror s3-cleanup --s3.enable --s3.bucket=my-bucket --s3.prefix=firmirror
```

This resolves those pairs, keeping the newer release of each, and then deletes
every `.cab` object under the prefix that neither the index, nor a ring
snapshot, nor a ring feed points at. A package only an older ring still serves
is kept, even once the index has moved past it. Metadata and non-CAB objects
are never deleted. Local storage does not support it and says so: a local
repository keeps its old packages.

A package is only reclaimed once it is older than `--min-package-age`, one day
by default. A `refresh` uploads each cabinet as it builds it and publishes the
metadata naming them only when the whole run ends, so for the length of a run
its cabinets sit in storage referenced by nothing. Deleting those would take
away what the run is about to publish. Anything younger than the threshold is
left for a later cleanup, by when the run has either published it or is long
over and it really is an orphan. Firmirror also takes a repository lock for
the whole refresh or cleanup, so two commands cannot race their metadata and
package changes. S3 locks are renewable leases and abandoned leases expire.
Losing the lease stops the run, the metadata commit it ends with included.

The S3 lock relies on conditional writes (`If-None-Match` and `If-Match` on
`PutObject`), which older S3-compatible endpoints do not implement. `--no-lock`
runs without it, and then nothing but the operator prevents two firmirror
commands from writing the same repository at once.

## Metadata Signing

Firmirror can sign LVFS metadata using the JCAT format, which is compatible with fwupd's signature verification.

1. **JCAT File Creation**: After compressing the metadata (`metadata.xml.zst`), a `.jcat` file is created
2. **Checksums**: The JCAT file always includes SHA256 checksums for integrity verification
3. **Digital Signature**: If certificate and private key are provided, the metadata is also signed with PKCS#7

### Generating a Self-Signed Certificate

```bash
contrib/makecert.sh
# Produces cert.pem and key.pem using GnuTLS certtool

./firmirror refresh /output/dir \
  --hpe.enable --hpe.gens=gen11 \
  --sign.certificate=cert.pem \
  --sign.private-key=key.pem
```

For production use, obtain certificates from a trusted Certificate Authority.

## Kubernetes Deployment

A Helm chart is available in `chart/` for deploying Firmirror as a CronJob.

```bash
helm install firmirror ./chart \
  --set vendors.hpe.enabled=true \
  --set vendors.hpe.gens=gen10\,gen11 \
  --set vendors.dell.enabled=true \
  --set vendors.dell.machinesId=0C60\,0C61
```

See [`chart/README.md`](chart/README.md) for the full configuration reference.

### S3 Storage with Kubernetes

```bash
helm install firmirror ./chart \
  --set vendors.hpe.enabled=true \
  --set vendors.hpe.gens="gen11" \
  --set storage.s3.enabled=true \
  --set storage.s3.bucket=my-bucket \
  --set storage.s3.region=us-east-1 \
  --set storage.s3.secretName=aws-credentials
```

### Rings with Kubernetes

Each entry of the chart's `promote` list creates a CronJob that is suspended,
because a promotion is a deliberate act rather than something that should
happen on a schedule. `contrib/promote.sh` fires one:

```bash
contrib/promote.sh beta my-preprod-cluster   # check the hosts on beta, then
contrib/promote.sh beta my-prod-cluster
```

The `-publish` and `-s3-cleanup` CronJobs are suspended for the same reason.
`contrib/firmirror-job.sh` finds any of them by Helm labels and runs it, which
is what `promote.sh` wraps. Pass the optional namespace and Helm release when
they differ from `firmirror`:

```bash
contrib/firmirror-job.sh s3-cleanup my-prod-cluster
contrib/firmirror-job.sh s3-cleanup my-prod-cluster firmware my-release
contrib/firmirror-job.sh publish    my-prod-cluster
```

It waits for the Job and prints its logs. Firmirror's shared repository lock
prevents it from racing the nightly refresh or another on-demand operation.

## Vendor Details

### Dell

Fetches firmware from the Dell DSU catalog (`https://dl.dell.com/catalog/catalog.xml.gz`). Only firmware entries (ComponentType `FRMW`) are included; drivers are excluded.

Machine IDs are 4-character hexadecimal codes representing Dell machine types (e.g. `0C60` corresponds to the PowerEdge C6615 series). You can specify multiple IDs by repeating the flag. If no machine IDs are specified, **all** Dell firmware is included (this can take a very long time).

```bash
# Filter to specific machine types
./firmirror refresh /output --dell.enable --dell.machines-id=0C60 --dell.machines-id=0C61

# No filter: fetch all Dell firmware
./firmirror refresh /output --dell.enable
```

### HPE

Fetches firmware from HPE SDR repositories (`https://downloads.linux.hpe.com/SDR/repo/`). Supports Gen8 through Gen12 servers. Only `.fwpkg` packages are included (the LVFS-compatible format for HPE firmware).

## Development

### Running Tests

```bash
go test -v ./...
```

Note: Some tests require `fwupdtool` and `jcat-tool` to be installed.

### Project Structure

```
.
├── cmd/firmirror.go              # CLI entry point
├── pkg/
│   ├── firmirror/                # Core sync logic, storage backends
│   │   ├── firmirror.go          # FirmirrorSyncer: orchestration
│   │   ├── vendor.go             # Vendor/Catalog/FirmwareEntry interfaces
│   │   ├── storage.go            # Storage interface
│   │   ├── storage_local.go      # Local filesystem storage
│   │   ├── storage_s3.go         # S3 storage backend
│   │   ├── metadata.go           # Reading, encoding and signing metadata
│   │   ├── cleanup.go            # Reclaiming superseded releases and packages
│   │   └── rings.go              # Ring snapshots, feeds and the blocklist
│   ├── lvfs/                     # LVFS AppStream XML types
│   ├── vendors/
│   │   ├── dell/                 # Dell vendor implementation
│   │   └── hpe/                  # HPE vendor implementation
│   └── utils/                    # Shared utilities (HTTP download, etc.)
├── chart/                        # Helm chart for Kubernetes deployment
├── contrib/                      # Helper scripts (certificates, promotion, cleanup)
└── Dockerfile                    # Multi-stage Docker build
```

## License

[Apache License 2.0](LICENSE)
