#!/usr/bin/env bash
# Promote a firmirror ring: copy the state its source ring is serving into it,
# so the hosts on that ring start getting firmware that has already been
# running on the ring before it.
#
#   contrib/promote.sh beta kubes10-fr4.preprod
#
# Which ring is promoted from which is configured in the chart values
# (promote[].from), not here. Promote in preprod first, let the hosts on that
# ring run the new firmware, then promote the same ring in prod.
set -euo pipefail

if [ $# -lt 2 ] || [ $# -gt 4 ]; then
  echo "usage: $0 <ring> <kube-context> [namespace] [helm-release]" >&2
  exit 2
fi

ring=$1
shift

exec "$(dirname "$0")/firmirror-job.sh" "promote-$ring" "$@"
