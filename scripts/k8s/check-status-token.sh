#!/bin/sh
# The status page's token (phase 2, G6) is written in four places: the two
# Ingress paths and the root redirect (deployments/k8s/monitoring/status.yaml)
# and the publisher's STATUS_TOKEN (grafana.yaml). If they differ, the page
# is a 404 or shows nothing, and no schema check notices. Fails unless the
# rendered monitoring has one token, in all four places.
set -eu
# Every standalone 32-hex-digit value in the render (image digests are 64).
found=$(kubectl kustomize deployments/k8s/monitoring |
  grep -oE '(^|[^0-9a-f])[0-9a-f]{32}([^0-9a-f]|$)' | grep -oE '[0-9a-f]{32}' || true)
n=$(printf '%s\n' "$found" | grep -c . || true)
distinct=$(printf '%s\n' "$found" | sort -u | grep -c . || true)
if [ "$distinct" -ne 1 ] || [ "$n" -ne 4 ]; then
  echo "status token: $distinct distinct value(s) in $n place(s); want 1 in 4" >&2
  exit 1
fi
echo "status token: one value in 4 places"
