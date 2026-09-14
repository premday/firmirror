# Firmirror Helm Chart

This Helm chart deploys Firmirror as a Kubernetes CronJob to periodically sync firmware from hardware vendor sources (Dell and HPE) and create an LVFS-compatible firmware repository.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- PersistentVolume provisioner support (optional, for local storage)
- S3 bucket access (optional, for S3 storage)
- External Secrets Operator (optional, for secure credential management)
- Container image with firmirror binary, fwupdtool and jcat-tool

## Installing the Chart

### Basic Installation

```bash
# Install with default configuration (no vendors enabled by default)
helm install firmirror ./chart

# Install with custom values
helm install firmirror ./chart \
  --set vendors.dell.enabled=true \
  --set vendors.dell.machinesId="0C60,0C61" \
  --set vendors.hpe.enabled=true \
  --set vendors.hpe.gens="gen10,gen11"
```

## Rings

Set `promote` to publish a progressive rollout. Each entry creates a suspended
CronJob that copies the state of its `from` ring into it, so a ring serves the
firmware that has already been running on the ring before it:

```yaml
promote:
- name: beta      # from the index: the latest mirrored firmware
- name: preview
  from: beta
- name: stable
  from: preview
```

Clients then point their fwupd remote at `metadata-<name>.xml.zst` instead of
`metadata.xml.zst`. Run a promotion with
`contrib/promote.sh <ring> <context> [namespace] [helm-release]`, which is what
fires the otherwise suspended CronJob.

`contrib/firmirror-job.sh <job> <context> [namespace] [helm-release]` finds any
of the suspended CronJobs by Helm labels, runs it, waits for it and prints its
logs. Firmirror's shared repository lock prevents it from racing the nightly
refresh or another on-demand operation.

`blocklist` withholds firmware from every ring feed. It takes effect on the
next nightly run or promotion; to apply it immediately, run the `-publish`
CronJob that is rendered alongside the promote ones.

A suspended `-s3-cleanup` CronJob is rendered whenever S3 storage is enabled.
It resolves the releases a vendor rebuild superseded and deletes the packages
nothing references, and is never run on a schedule: mirroring firmware neither
drops a release nor deletes a package by itself. It leaves packages younger
than a day alone, so it cannot take away what a refresh in progress is about
to publish.

See the repository README for what each object in the bucket is for.

## Configuration

The following table lists the configurable parameters of the Firmirror chart and their default values.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `schedule` | Cron schedule for the job | `"0 2 * * *"` (2 AM daily) |
| `nameOverride` | Override the chart name | `""` |
| `fullnameOverride` | Override the full release name | `""` |
| `image.repository` | Container image repository | `ghcr.io/premday/firmirror` |
| `image.tag` | Container image tag | `""` (Chart appVersion) |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Image pull secrets | `[]` |
| `blocklist` | Firmware withheld from the published ring feeds, as vendor firmware filenames or CAB names | `[]` |
| `promote` | Rings to publish, as `{name, from}` entries; one suspended promote CronJob each | `[]` |
| `vendors.dell.enabled` | Enable Dell firmware sync | `false` |
| `vendors.dell.machinesId` | Comma-separated Dell machine System IDs | `""` |
| `vendors.hpe.enabled` | Enable HPE firmware sync | `false` |
| `vendors.hpe.gens` | Comma-separated HPE generations (gen10,gen11,gen12) | `""` |
| `storage.outputDir` | Output directory inside container (for local storage) | `/data/firmirror` |
| `storage.s3.enabled` | Enable S3 storage backend | `false` |
| `storage.s3.bucket` | S3 bucket name | `""` |
| `storage.s3.prefix` | S3 prefix/path within bucket | `""` |
| `storage.s3.region` | AWS region | `""` |
| `storage.s3.endpoint` | Custom S3 endpoint (for MinIO, etc.) | `""` |
| `storage.s3.lock` | Take the repository lock, which needs conditional writes; turn it off for an endpoint without them | `true` |
| `storage.s3.secretName` | Secret containing AWS credentials | `""` |
| `externalSecret.create` | Create an ExternalSecret resource | `false` |
| `externalSecret.secretStoreRef` | Reference to the SecretStore | `""` |
| `externalSecret.targetSecret` | Name of the secret to create | `""` |
| `externalSecret.data` | Data mapping configuration | `[]` |
| `signing.enabled` | Enable metadata signing with JCAT | `false` |
| `signing.secretName` | Name of secret containing signing certificate and key | `""` |
| `signing.certKey` | Key name in secret for certificate file | `""` |
| `signing.pkeyKey` | Key name in secret for private key file | `""` |
| `persistence.enabled` | Enable persistent storage (only for local storage) | `false` |
| `persistence.existingClaim` | Use existing PVC | `""` |
| `persistence.storageClass` | Storage class name | `""` (default class) |
| `persistence.accessMode` | PVC access mode | `ReadWriteOnce` |
| `persistence.size` | PVC size | `50Gi` |
| `cronjob.successfulJobsHistoryLimit` | Number of successful jobs to keep | `3` |
| `cronjob.failedJobsHistoryLimit` | Number of failed jobs to keep | `3` |
| `cronjob.restartPolicy` | Pod restart policy | `OnFailure` |
| `cronjob.backoffLimit` | Number of retries before marking job as failed | `2` |
| `cronjob.activeDeadlineSeconds` | Maximum refresh runtime in seconds | `7200` (2 hours) |
| `jobs.activeDeadlineSeconds` | Maximum runtime of an on-demand job (promote, publish, s3-cleanup) | `1800` (30 minutes) |
| `jobs.resources` | Resources for the on-demand jobs | `500m` CPU, `1Gi` memory |
| `cronjob.ttlSecondsAfterFinished` | Time to keep finished jobs | `86400` (24 hours) |
| `resources.limits.cpu` | CPU limit | `2000m` |
| `resources.limits.memory` | Memory limit | `4Gi` |
| `resources.requests.cpu` | CPU request | `500m` |
| `resources.requests.memory` | Memory request | `1Gi` |
| `podSecurityContext.fsGroup` | Pod fsGroup | `1000` |
| `podSecurityContext.runAsUser` | Pod user ID | `1000` |
| `podSecurityContext.runAsNonRoot` | Run as non-root | `true` |
| `securityContext.allowPrivilegeEscalation` | Allow privilege escalation | `false` |
| `securityContext.capabilities.drop` | Dropped capabilities | `["ALL"]` |
| `securityContext.readOnlyRootFilesystem` | Read-only root filesystem | `false` |
| `securityContext.runAsNonRoot` | Run as non-root | `true` |
| `securityContext.runAsUser` | Container user ID | `1000` |
| `serviceAccount.create` | Create service account | `false` |
| `serviceAccount.annotations` | Service account annotations | `{}` |
| `serviceAccount.name` | Service account name | `""` |

## License

See the main project repository for license information.
