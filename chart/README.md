# Firmirror Helm Chart

This Helm chart deploys Firmirror as a Kubernetes CronJob, or as a daemon (see [Daemon](#daemon)), to periodically sync firmware from hardware vendor sources (Dell and HPE) and create an LVFS-compatible firmware repository.

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
| `daemon.enabled` | Run `firmirror daemon` as a Deployment instead of the refresh and `regen-metadata` CronJobs, which then do not exist | `false` |
| `daemon.refreshInterval` | Time from the end of a refresh to the next one | `24h` |
| `daemon.bkcPollInterval` | How often `bkc.url` is read to see whether it changed | `15m` |
| `daemon.retryInterval` | Time to the retry of a failed refresh or regeneration, doubled each time it fails again | `15m` |
| `daemon.refreshTimeout` | Time after which a refresh is stopped, and what it mirrored published | `12h` |
| `daemon.regenTimeout` | Time after which a regeneration is stopped, and tried again | `1h` |
| `daemon.terminationGracePeriodSeconds` | Time to finish a publish on termination | `600` |
| `daemon.extraArgs` | More arguments of the daemon, such as `--force` (it also gets `extraArgs`) | `[]` |
| `daemon.metrics.port` | Port of `/metrics` and `/healthz` | `9090` |
| `daemon.metrics.service.enabled` | A Service for the metrics | `false` |
| `daemon.metrics.podMonitor.enabled` | A PodMonitor for the Prometheus Operator, with `interval` and `labels` | `false` |
| `schedule` | Cron schedule for the job. Not used by the daemon | `"0 2 * * *"` (2 AM daily) |
| `nameOverride` | Override the chart name | `""` |
| `fullnameOverride` | Override the full release name | `""` |
| `image.repository` | Container image repository | `ghcr.io/premday/firmirror` |
| `image.tag` | Container image tag | `""` (Chart appVersion) |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Image pull secrets | `[]` |
| `vendors.dell.enabled` | Enable Dell firmware sync | `false` |
| `vendors.dell.machinesId` | Comma-separated Dell machine System IDs | `""` |
| `vendors.dell.baseUrl` | Copy of `https://dl.dell.com` to read from (URL, `file://` URL or directory) | `""` |
| `vendors.hpe.enabled` | Enable HPE firmware sync | `false` |
| `vendors.hpe.gens` | Comma-separated HPE generations (gen10,gen11,gen12) | `""` |
| `vendors.hpe.baseUrl` | Copy of `https://downloads.linux.hpe.com/SDR/repo` to read from (URL, `file://` URL or directory) | `""` |
| `storage.outputDir` | Output directory inside container (for local storage) | `/data/firmirror` |
| `storage.s3.enabled` | Enable S3 storage backend | `false` |
| `storage.s3.cleanup` | Replace rebuilt metadata releases and delete unreferenced CAB packages | `false` |
| `storage.s3.bucket` | S3 bucket name | `""` |
| `storage.s3.prefix` | S3 prefix/path within bucket | `""` |
| `storage.s3.region` | AWS region | `""` |
| `storage.s3.endpoint` | Custom S3 endpoint (for MinIO, etc.) | `""` |
| `storage.s3.secretName` | Secret containing AWS credentials | `""` |
| `firmwareBaseUrl` | Public URL the output is served from (the remote's `FirmwareBaseURI`); packages are then named by absolute URL in the metadata. Passed to the refresh and the `regen-metadata` Job alike | `""` (bare file names) |
| `bkc.url` | Where the refresh and the `regen-metadata` Job read what each rollout ring serves, an HTTP(S) URL (plain HTTP needs `bkc.allowHttp`). Empty publishes no ring tag | `""` |
| `bkc.allowHttp` | Also read it from a plain HTTP URL, for a network that is trusted | `false` |
| `extraArgs` | More arguments of the refresh, such as `--force` | `[]` |
| `bkc.regen.extraArgs` | More arguments of the `regen-metadata` Job, such as `--force` | `[]` |
| `bkc.regen.ttlSecondsAfterFinished` | Time to keep a finished `regen-metadata` Job, whose logs are the record of what was published. 0 deletes it as soon as it finishes, unlike for the refresh, where 0 sets no TTL | `604800` (7 days) |
| `bkc.regen.historyLimit` | Finished `regen-metadata` Jobs kept, successful and failed each, whatever their TTL. 0 keeps none | `10` |
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

## Daemon

With `daemon.enabled=true` the chart creates one Deployment that runs `firmirror daemon`, and neither the
refresh CronJob nor the `regen-metadata` one. It refreshes at startup and publishes the metadata signed
whether or not it changed, publishes it again as soon as `bkc.url` changes (read every
`daemon.bkcPollInterval`), refreshes every `daemon.refreshInterval`, and serves Prometheus metrics, see the
main README. `schedule`, `cronjob.*` and `bkc.regen.*` are not used.

It has one replica, and the Deployment is replaced only after the old pod stopped (`Recreate`), as the
store has one writer. When switching from the CronJobs, upgrade the release with `daemon.enabled=true`, which
removes them, let a Job that is running finish first, and do not start one in between: a Job and the daemon publishing at once can
replace one another's metadata. A pod that is replaced during a long refresh publishes what it mirrored so
far, and the next one mirrors the rest; with `persistence` or S3 nothing mirrored is lost, and with the
default `emptyDir` everything is mirrored again.

The daemon signs with the keys as the mounted Secret holds them, at each publish, and signs an unchanged
document again only at startup: to publish with a new key without waiting for a change, restart the
Deployment (`kubectl rollout restart`). The chart sets
`--daemon.metrics-address` from `daemon.metrics.port`, which the probes use, so do not override it in
`daemon.extraArgs`. A startup probe gives it five minutes to read its storage and the rings, and the pod is
restarted past that. There is no liveness probe: `/healthz` answers for as long as the process runs, so it
would detect nothing, and could restart the pod in the middle of a refresh whose work is lost until it is
published.

## Rollout rings

With `bkc.url` set, the refresh reads a YAML file saying which version of each
component every rollout ring serves, and tags the releases it publishes
accordingly. A host joins a ring with `HostBkc=<tag prefix>-<ring>` in `fwupd.conf`
followed by `fwupdmgr sync`; nothing else is deployed to the hosts. The file is
edited with `firmirror bkc promote`, outside the cluster, and reviewed like any
other change, see the main README. A refresh that cannot read it fails and is
retried.

Publishing the metadata again without mirroring anything, to publish new
definitions now and not at the next refresh, or for a new signing key, which the
refresh would not publish, is an action and not a schedule. The chart creates a
**suspended CronJob** named `<fullname>-regen-metadata`, with the name of the
refresh CronJob as `<fullname>`, when the storage is S3 or a volume. A
`<fullname>` over 37 characters is cut short, as Kubernetes limits a CronJob
name to 52 (the notes printed by `helm install` give the exact name). It is built
from the same Job spec as the refresh, with the same image, storage, credentials,
signing keys and base URL, because it publishes the metadata the refresh
publishes. Start it as a Job:

```bash
kubectl create job --from=cronjob/firmirror-regen-metadata regen-metadata-$(date +%s)
```

It is never retried, whatever `cronjob.backoffLimit` and `cronjob.restartPolicy`
say for the refresh. `concurrencyPolicy: Forbid` stops a CronJob from overlapping
itself, not another CronJob: do not start it while a refresh is running.
