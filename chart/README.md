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
| `vendors.dell.enabled` | Enable Dell firmware sync | `false` |
| `vendors.dell.machinesId` | Comma-separated Dell machine System IDs | `""` |
| `vendors.hpe.enabled` | Enable HPE firmware sync | `false` |
| `vendors.hpe.gens` | Comma-separated HPE generations (gen10,gen11,gen12) | `""` |
| `storage.outputDir` | Output directory inside container (for local storage) | `/data/firmirror` |
| `storage.s3.enabled` | Enable S3 storage backend | `false` |
| `storage.s3.cleanup` | Replace rebuilt metadata releases and delete unreferenced CAB packages | `false` |
| `storage.s3.bucket` | S3 bucket name | `""` |
| `storage.s3.prefix` | S3 prefix/path within bucket | `""` |
| `storage.s3.region` | AWS region | `""` |
| `storage.s3.endpoint` | Custom S3 endpoint (for MinIO, etc.) | `""` |
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
| `cronjob.activeDeadlineSeconds` | Maximum job runtime in seconds | `7200` (2 hours) |
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
