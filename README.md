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
- **Concurrent Downloads**: Configurable concurrency for downloading and processing firmware entries
- **Pluggable Storage**: Local filesystem or S3 (including S3-compatible services like MinIO)
- **Metadata Signing**: JCAT signatures with SHA256 checksums and optional PKCS#7 X.509 signatures
- **Helm Chart**: Kubernetes CronJob deployment with S3/local storage support

## Prerequisites

- Go 1.25+
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
| **Dell** | | |
| `--dell.enable` | Enable Dell firmware mirroring | `false` |
| `--dell.machines-id` | Machine System IDs to filter (4-char hex, e.g. `0C60`). Can be specified multiple times. Omit to include all firmware | (optional) |
| **HPE** | | |
| `--hpe.enable` | Enable HPE firmware mirroring | `false` |
| `--hpe.gens` | Generations to fetch (`gen8`–`gen12`) | `gen8,gen9,gen10,gen11,gen12` |
| **S3 Storage** | | |
| `--s3.enable` | Use S3 storage backend instead of local filesystem | `false` |
| `--s3.cleanup` | Replace rebuilt releases in metadata and delete unreferenced CAB packages from S3 after a successful metadata save | `false` |
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
├── metadata.xml.zst              # Compressed LVFS metadata
└── metadata.xml.zst.jcat         # JCAT signature/checksum file
```

The `metadata.xml.zst` file contains the AppStream component metadata that `fwupd` uses to discover available firmware. The `.jcat` file provides integrity verification and optional signatures.

### Incremental Updates

Firmirror tracks which firmware files have already been processed in the metadata index. On subsequent runs, already-existing firmware is skipped, so only new entries are downloaded and processed.

To force re-processing of a firmware, delete its CAB file and remove it from the metadata.

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
  --s3.cleanup \
  --s3.bucket=my-bucket \
  --s3.prefix=firmirror \
  --s3.region=eu-west-1 \
  --hpe.enable --hpe.gens=gen11
```

S3 cleanup is opt-in. With `--s3.cleanup`, Firmirror replaces old metadata releases when their packages are rebuilt, then deletes every `.cab` object under the configured prefix that is not referenced by the updated metadata. Without the flag, rebuilt releases are appended and existing CAB objects are retained. Metadata and non-CAB objects are never deleted.

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
│   │   └── storage_s3.go         # S3 storage backend
│   ├── lvfs/                     # LVFS AppStream XML types
│   ├── vendors/
│   │   ├── dell/                 # Dell vendor implementation
│   │   └── hpe/                  # HPE vendor implementation
│   └── utils/                    # Shared utilities (HTTP download, etc.)
├── chart/                        # Helm chart for Kubernetes deployment
├── contrib/                      # Helper scripts (certificate generation)
└── Dockerfile                    # Multi-stage Docker build
```

## License

[Apache License 2.0](LICENSE)
