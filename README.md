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

- Go 1.26+
- `fwupdtool` (for building CAB packages)
- `jcat-tool` (for creating JCAT signature files)

On Debian/Ubuntu:

```bash
sudo apt install fwupd jcat
```

Downloads, and the reads of the rollout rings, honour `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`: list a mirror on the local network in `NO_PROXY`.

## Installation

### From Source

```bash
go build -o firmirror ./cmd
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
| `--firmware-base-url` | Public URL the output is served from (the remote's `FirmwareBaseURI`). Packages are then named by absolute URL in the metadata | `""` (bare file names) |
| `--concurrency` | Max concurrent firmware downloads per vendor | `8` |
| **Dell** | | |
| `--dell.enable` | Enable Dell firmware mirroring | `false` |
| `--dell.machines-id` | Machine System IDs to filter (4-char hex, e.g. `0C60`). Can be specified multiple times. Omit to include all firmware | (optional) |
| `--dell.base-url` | Copy of `https://dl.dell.com` to read from: an HTTP(S) URL, a `file://` URL, or a directory | (the site itself) |
| **HPE** | | |
| `--hpe.enable` | Enable HPE firmware mirroring | `false` |
| `--hpe.gens` | Generations to fetch (`gen8`–`gen12`) | `gen8,gen9,gen10,gen11,gen12` |
| `--hpe.base-url` | Copy of `https://downloads.linux.hpe.com/SDR/repo` to read from: an HTTP(S) URL, a `file://` URL, or a directory | (the SDR repository itself) |
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
| **Rollout rings** | | |
| `--bkc.file` | What each rollout ring serves: a YAML file, or the HTTP(S) URL it is fetched from (plain HTTP needs `--bkc.allow-http`). A refresh tags the releases it publishes accordingly, and publishes no ring tag without it. `bkc promote` edits it, so it is a path there | (no ring) |
| `--bkc.allow-http` | Also read the file, and the `--metadata` of `bkc promote`, from a plain HTTP URL, which whoever is on the way can change | `false` |
| `--bkc.rings` | The rings of a file `bkc promote` creates, in promotion order | `dev,beta,preview,stable` |

### Commands

| Command | Description |
|---------|-------------|
| `refresh` | Mirror the enabled vendors and publish the metadata |
| `regen-metadata` | Publish the stored metadata again, rendered with the current rollout rings and signed with the current keys, even if it did not change |
| `bkc status [--ring R]` | Report which version each rollout ring serves for each component, from `--bkc.file` |
| `bkc promote <ring>` | Move a ring on in the file `--bkc.file` names, which it creates if needed. Nothing is published |

### Output Structure

```
/output/dir/
├── <sha256>-firmware1.bin.cab    # CAB packages (prefixed with SHA256 hash)
├── <sha256>-firmware2.bin.cab
├── metadata.xml.zst              # Compressed LVFS metadata
└── metadata.xml.zst.jcat         # JCAT signature/checksum file
```

The `metadata.xml.zst` file contains the AppStream component metadata that `fwupd` uses to discover available firmware. The `.jcat` file provides integrity verification and optional signatures.

### Configuring the fwupd remote

Point a remote at the published directory in `/etc/fwupd/remotes.d/firmirror.conf`:

```ini
[fwupd Remote]
Enabled=true
MetadataURI=https://mirror.example.com/fwupd/metadata.xml.zst
FirmwareBaseURI=https://mirror.example.com/fwupd/
```

A remote that sets `FirmwareBaseURI` needs `--firmware-base-url=https://mirror.example.com/fwupd/` on
`firmirror refresh`. Without it the metadata holds bare file names, and fwupd fails the update with
`failed to parse url '<sha256>-<file>.cab'`. A remote without `FirmwareBaseURI` resolves the bare names
next to `MetadataURI` and needs no flag. Changing the flag rewrites the stored locations on the next run.

### Rollout rings

fwupd has a channel mechanism of its own. A host names one or more BKC tags in `HostBkc`, and
`fwupdmgr sync` moves each device to the release carrying a matching tag, downgrading when it has to.
Firmirror turns that into rollout rings, for example `dev`, `beta`, `preview` and `stable`: every ring is a
tag (`firmirror-dev`, `firmirror-beta`, ...), and the metadata it publishes tags each release with the rings
that serve it. One document and one remote serve every ring. A host joins a ring in
`/etc/fwupd/fwupd.conf`:

```ini
[fwupd]
HostBkc=firmirror-stable
```

and runs `fwupdmgr refresh && fwupdmgr sync`. Only `sync` follows the tags: `fwupdmgr update` and
`get-updates` still pick the newest release the remote offers.

**What each ring serves is a YAML file, and the metadata is only generated from it.** It is edited on a
workstation, reviewed and committed like any other change, and fetched by the refresh:

```yaml
version: 1
rings: [dev, beta, preview, stable]   # promotion order: the first takes the newest mirrored release
tag_prefix: firmirror                 # optional
components:
  - id: com.example.nic
    platform: 3a8f2c91e07b45d6        # a digest of the GUIDs: it tells platforms of one ID apart
    name: Example NIC                 # for whoever reads the file
    versions: {dev: "2.90", beta: "2.80", preview: "2.80", stable: "2.80"}
```

- `firmirror bkc promote dev --bkc.file bkc.yml --metadata <published metadata>` creates the file or moves
  `dev` on to the newest release of every mirrored component, adding the ones it has no entry for. The
  metadata is a path or a URL, such as the one the hosts read, or else the storage the command is given.
  Any other ring takes what the ring before it serves, and only for components that ring serves: promote
  `beta` after `dev`, `preview` after `beta`, `stable` after `preview`. It prints what it changes and writes
  the file, which it writes whole: comments in it are not kept. It refuses a promotion that moves a ring
  backwards unless `--allow-downgrade` says that is the point (`fwupdmgr sync` installs older releases too,
  so it downgrades the hosts on the ring, which is also how a ring is rolled back), and stops at
  `--dry-run`. A ring that serves a version the mirror no longer has cannot be placed: leaving it for the
  newest release is forward, and for any other it counts as backwards, as hosts may still run it.
  `--component` narrows it to some component IDs. `--prune` also removes the entries of components the
  mirror no longer has (not together with `--component`), and refuses to remove more than half of the file
  unless `--allow-mass-prune` says so, as a mirror that is partial or of another environment would.
  Not promoting a component is how it is held.
- `firmirror refresh --bkc.file <path or URL>` tags the releases it publishes as the file says. Newer
  firmware that the file does not name is served by no ring until it is promoted into `dev`. A refresh that
  cannot read the file fails, as it cannot publish rings without it, and reads it again just before it
  publishes, so that a promotion committed during a long run is not reverted by the file it started with.
  When that second read fails, or what it read would be refused as below, the run publishes what it
  mirrored with the file it started with, which the log says, and exits successfully: the promotion waits
  for the next refresh, or `regen-metadata`.
  Without `--bkc.file` it publishes no ring tag at all. It refuses to publish a document that lacks a ring
  tag the stored one has, as a forgotten or stale file, another tag prefix or a ring removed from the file
  would take the hosts on that ring off it. A ring still counts when some entry names a version for it,
  whatever the mirror holds: its hosts then stay where they are. It says so before it mirrors anything, and
  likewise refuses, before it mirrors anything, to take a signed JCAT off for want of signing keys, as
  one that cannot be read may be signed: `--force` says that is the point of the tags, and
  `--stop-signing` of not signing any more. It likewise refuses to take the base URL out of the locations of what is
  published, as a forgotten `--firmware-base-url` would: `--drop-base-url` says that is the point. The
  file is a trusted input, as it decides what every host is served: it is read over HTTPS, a plain HTTP
  URL needs `--bkc.allow-http`, and the URL is shown in the CronJob and the logs, so it should hold no
  credentials. It is read up to 16 MiB, and a field it does not know is ignored at its top when a URL
  serves it, as the service can be ahead of this firmirror, and an error anywhere else.
- `firmirror regen-metadata` publishes the stored document again, whether or not it changed: a refresh
  leaves an unchanged document alone, and a new signing key does not change it, although new definitions
  do, so the next refresh publishes those. Run it after the file changes, if the next refresh is too far
  away, and after switching to a new signing key. It
  needs the storage, signing, `--firmware-base-url` and `--bkc.file` arguments of the refresh, mirrors
  nothing, and refuses to publish without `--sign.*` over a JCAT that is signed. As it renders the stored
  document with only what it is given, it also refuses to take the ring tags or the base URL out of it, as
  a forgotten argument would, unless `--force` (the tags) or `--drop-base-url` says that is the point, nor stop signing unless `--stop-signing` says so. It goes on after a first signal, as
  ending between the JCAT and the metadata would leave one that is not the other's, and ends at the second.
  It fails, and publishes nothing, if
  the metadata was published while it ran.
- `firmirror bkc status` shows what each ring serves, from the file.

The Helm chart reads the file from `bkc.url`, and has a suspended CronJob that starts `regen-metadata`, see
its README.

Things to know:

- "Newest" is the release published last, by release date, not the highest version: vendor versions
  (`2.90`, `7.30.30.51`, `20.43.2566`, `HPG4`) are not ordered by anything firmirror could rely on. The
  same goes for what counts as a downgrade. Two releases of the same day are told apart by their timestamp
  when the vendor gives one (Dell and HPE do), and otherwise by checksum, which is stable but means
  nothing. A vendor re-issuing older firmware under a newer date would make `promote dev` pick it: look at
  the table before committing the file. A release with no date, which HPE publishes when neither the
  package nor the catalog has one, counts as the oldest, so it is the newest only for a component none of
  whose releases has a date: `promote dev` warns about it, and the file is where it is pinned. As nothing
  tells which of two releases is newer when one has no date, moving a ring off or onto an undated release
  counts as a downgrade, and `--allow-downgrade` with `--component` says that is the point.
- fwupd reads a tag from the *component* and gives it to every release in it, so a component whose
  releases are served by different rings is published as several components of the same ID and GUIDs,
  one per set of tags. fwupd merges the releases of every component matching a device.
- Rings work per component, an ID and its GUIDs. fwupd resolves a device across every component
  providing its GUID, and `fwupdmgr sync` takes the newest tagged release among them, so a device whose
  GUID two components provide can be moved by the other one. Several Dell platforms share GUIDs this way.
- A version the file names that is no longer mirrored leaves its ring serving nothing for that component,
  which the refresh logs, as it does the components the file has no entry for.
- Two runs publishing at once can still replace one another's metadata: `concurrencyPolicy: Forbid` does
  not stop a Job created by hand from a CronJob.

### Incremental Updates

Firmirror tracks which firmware files have already been processed in the metadata index. On subsequent runs, already-existing firmware is skipped, so only new entries are downloaded and processed.

To force re-processing of a firmware, delete its CAB file and remove it from the metadata.

### When the metadata is published

The metadata is only written when it has to be. A run that mirrors nothing leaves an identical `metadata.xml.zst` and its JCAT alone: signing again would change the JCAT, which every client compares to decide whether to download the metadata again. It is published again when the JCAT is not the metadata's, as an interrupted write leaves it (the JCAT is written first, the metadata last), or when it holds no signature while signing is configured, so turning signing on takes effect at the next refresh without waiting for new firmware. A signed JCAT is never replaced by one holding only checksums unless `--stop-signing` says so: a run without signing keys fails before it mirrors anything, as the keys are probably missing.

If another run published meanwhile, a run that mirrors nothing leaves that document alone, and removes no package either. A refresh that cannot save the metadata fails. Do not run two refreshes at once: `concurrencyPolicy: Forbid` does not stop a Job created by hand from the CronJob, and a run that mirrors new firmware still replaces what another run published. Changing the signing key does not sign an unchanged document again, as a signature is only looked for, not checked.

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

### Reading from a local mirror

Both vendors publish their repository over rsync as well as over HTTP, as
[HPE](https://downloads.linux.hpe.com/) documents, so a site that already keeps
a copy can mirror from that copy instead of from the vendor. `--hpe.base-url`
and `--dell.base-url` take whatever the copy is reachable as: an HTTP(S) URL, a
`file://` URL, or a plain directory, which is read where it sits with no web
server in front of it. A `file://` URL names the path as `file:///absolute/path`
or `file:relative/path`; one that names another host is refused.

```bash
# Mirror from directories kept up to date with rsync
./firmirror refresh /output \
  --hpe.enable --hpe.gens=gen11 --hpe.base-url=/srv/mirror/SDR/repo \
  --dell.enable --dell.machines-id=0C60 --dell.base-url=/srv/mirror/dell

# Or from an internal web server
./firmirror refresh /output \
  --hpe.enable --hpe.gens=gen11 --hpe.base-url=https://mirror.example.com/SDR/repo
```

The HPE base is the directory the `fwpp-<gen>` repositories sit in; the Dell one
is the directory `catalog/catalog.xml.gz` sits in.

Where the firmware is read from does not change what is published: the metadata
keeps pointing at the vendor's own location, which is the one a host reading the
repository can follow. Switching an existing mirror to a local copy therefore
rewrites nothing and costs no re-mirror. The firmware itself is still copied
into a temporary directory to be repackaged, so the mirror is only ever read.

## Development

### Running Tests

```bash
go test -v ./...
```

Note: Some tests require `fwupdtool` and `jcat-tool` to be installed.

### Project Structure

```
.
├── cmd/
│   ├── firmirror.go              # CLI entry point, flags, shared helpers
│   ├── refresh.go                # refresh
│   ├── bkc.go                    # bkc status, bkc promote
│   └── regen.go                  # regen-metadata
├── pkg/
│   ├── firmirror/                # Core sync logic, storage backends
│   │   ├── firmirror.go          # FirmirrorSyncer: orchestration, publishing the metadata
│   │   ├── jcat.go               # Reading the stored JCAT
│   │   ├── baseurl.go            # Package locations, bare or under --firmware-base-url
│   │   ├── regen.go              # regen-metadata, reading published metadata
│   │   ├── vendor.go             # Vendor/Catalog/FirmwareEntry interfaces
│   │   ├── storage.go            # Storage interface
│   │   ├── storage_local.go      # Local filesystem storage
│   │   └── storage_s3.go         # S3 storage backend
│   ├── bkc/                      # Rollout rings: the definitions file, tagging, promotion
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
