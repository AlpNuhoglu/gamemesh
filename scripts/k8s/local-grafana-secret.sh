#!/bin/sh
# Creates (or updates) the grafana-admin Secret in the local cluster's
# monitoring namespace from GRAFANA_ADMIN_PASSWORD in the local overlay's
# .env.local (written by local-secrets.sh). In production the admin creates it
# by hand (docs/deployment.md, "Monitoring"). The value goes through a pipe,
# never through a command line or the terminal.
set -eu
file=deployments/k8s/overlays/local/.env.local
grep -q '^GRAFANA_ADMIN_PASSWORD=' "$file" || { echo "no GRAFANA_ADMIN_PASSWORD in $file; run make k8s-secrets" >&2; exit 1; }
sed -n 's/^GRAFANA_ADMIN_PASSWORD=//p' "$file" | tr -d '\n' |
  kubectl -n monitoring create secret generic grafana-admin --from-file=password=/dev/stdin \
    --dry-run=client -o yaml | kubectl apply -f -
