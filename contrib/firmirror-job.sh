#!/usr/bin/env bash
# Run one of firmirror's suspended CronJobs: a promotion, a feed republish, or
# a storage cleanup. They are suspended because each one is a deliberate act
# rather than something that should happen on a schedule, so this fires a Job
# off the CronJob's template and reports what it did.
#
#   contrib/firmirror-job.sh s3-cleanup kubes10-fr4.preprod
#   contrib/firmirror-job.sh publish    kubes10-fr4.preprod
#
# See contrib/promote.sh for promotions, which wraps this.
set -euo pipefail

usage() {
  echo "usage: $0 <job> <kube-context> [namespace] [helm-release]" >&2
  echo "  job: s3-cleanup, publish, or promote-<ring>" >&2
  exit 2
}

[ $# -ge 2 ] && [ $# -le 4 ] || usage
job=$1
context=$2
namespace=${3:-firmirror}
release=${4:-firmirror}

kube() { kubectl --context "$context" -n "$namespace" "$@"; }

operation=$job
selector="app.kubernetes.io/instance=$release,firmirror.premday.io/operation=$operation"
if [[ $job == promote-* ]]; then
  operation=promote
  ring=${job#promote-}
  ring_hash=$(printf '%s' "$ring" | sha256sum | cut -c1-16)
  selector="app.kubernetes.io/instance=$release,firmirror.premday.io/operation=$operation,firmirror.premday.io/ring-hash=$ring_hash"
fi
mapfile -t cronjobs < <(kube get cronjobs -l "$selector" -o name)
if [ "${#cronjobs[@]}" -ne 1 ]; then
  echo "expected one CronJob matching $selector in $context/$namespace, found ${#cronjobs[@]}" >&2
  exit 1
fi
cronjob=${cronjobs[0]#*/}

# A Job name has to be a lowercase DNS-1123 subdomain, while a ring name may
# carry uppercase letters and underscores that firmirror accepts. Normalize it
# the way the chart normalizes the CronJob name, hash included so two ring
# names cannot collide once sanitized.
safe=$(printf '%s' "$job" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9.-]+/-/g; s/^[-.]+//; s/[-.]+$//')
if [ "$safe" != "$job" ] || [ ${#safe} -gt 38 ]; then
  safe="$(printf '%s' "${safe:0:38}" | sed -E 's/[-.]+$//')-$(printf '%s' "$job" | sha256sum | cut -c1-8)"
fi

name="$safe-$(date +%Y%m%d-%H%M%S)"
echo "=== $context/$namespace: $name"
kube create job "$name" --from="cronjob/$cronjob"

# Wait on the Job's conditions rather than on its pod counters: a Job that
# never gets a pod scheduled leaves both .status.succeeded and .status.failed
# unset, and a first pod failure sets .status.failed while backoffLimit still
# has retries left, which is not a failed job yet. Complete and Failed are the
# terminal conditions, the one activeDeadlineSeconds sets included.
timeout=${FIRMIRROR_JOB_TIMEOUT:-3600}
deadline=$((SECONDS + timeout))
result=timeout
while [ "$SECONDS" -lt "$deadline" ]; do
  conditions=$(kube get job "$name" -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{"\n"}{end}')
  case $conditions in
    *Complete*)
      result=complete
      break
      ;;
    *Failed*)
      result=failed
      break
      ;;
  esac
  sleep 5
done

# A Job that never ran a pod has no logs, which must not hide the verdict.
kube logs "job/$name" || true

case $result in
  complete)
    echo "$job finished in $context"
    ;;
  failed)
    echo "$name failed" >&2
    exit 1
    ;;
  *)
    echo "$name did not finish within ${timeout}s and is still running in $context/$namespace" >&2
    exit 1
    ;;
esac
