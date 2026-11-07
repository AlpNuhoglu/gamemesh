#!/bin/bash
# The k3s datastore's nightly backup (phase 2, E3). Install as
# /usr/local/sbin/gamemesh-k3s-backup; gamemesh-k3s-backup.timer runs it as
# root (docs/deployment.md, "Backups").
#
#   sqlite3 .backup -> integrity check -> gzip -> pod (upload) -> age -> R2
#
# The datastore holds every Kubernetes object and, encrypted with the server
# token, the cluster's CA keys and the Secrets' encryption key (k3s's
# bootstrap data). The token is never uploaded: it is kept offline by the
# admin, so the bucket and the age key together still do not open the
# Secrets (finding #80).
#
# The host only reads the datastore. Encrypting and uploading run in a pod
# of deployments/backup's image, the version the Postgres backup CronJob
# runs now, under the namespace's restricted rules; the copy reaches it on
# standard input. So the host carries no age or rclone of its own, and this
# script needs no capability at all. (k3s ctr, tried first, mounts the
# image's root file system to resolve its user, which needs CAP_SYS_ADMIN.)
#
# The pod reads the gamemesh-k3s-backup Secret, created by the admin:
#   RCLONE_CONFIG_R2_ACCESS_KEY_ID, RCLONE_CONFIG_R2_SECRET_ACCESS_KEY
#                          a second R2 token, this bucket only (Object Read & Write)
#   RCLONE_CONFIG_R2_ENDPOINT  https://<account id>.r2.cloudflarestorage.com
#   AGE_RECIPIENT          the admin's age public key (age1...)
#   PING_URL               optional: the dead man's switch (phase 2, F2)
set -euo pipefail

db=${K3S_DB:-/var/lib/rancher/k3s/server/db/state.db}
dest=${BACKUP_DEST:-r2:gamemesh-backups/k3s}
work=${STATE_DIRECTORY:-/var/lib/gamemesh-k3s-backup}
stamp=$(date -u +%Y-%m-%dT%H%M%SZ)
snap="$work/state-$stamp.db"
name="k3s-$stamp.db.gz"
pod="k3s-backup-$(date +%s)"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

cleanup() {
  rm -f "$snap" "$snap.gz"
  k3s kubectl -n gamemesh delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1
  return 0
}
trap cleanup EXIT

# A consistent copy while k3s keeps writing: SQLite's online backup takes
# the database as of one transaction, its write-ahead log included. Copying
# the files with cp can catch the log half written.
sqlite3 -readonly "$db" ".backup '$snap'"
check=$(sqlite3 "$snap" 'PRAGMA integrity_check')
if [ "$check" != ok ]; then
  echo "integrity check of the copy: $check" >&2
  exit 1
fi
rows=$(sqlite3 "$snap" 'SELECT count(*) FROM kine')
# Without its bootstrap entry the copy restores no cluster.
if [ "$(sqlite3 "$snap" "SELECT count(*) FROM kine WHERE name LIKE '/bootstrap/%'")" -lt 1 ]; then
  echo "the copy has no bootstrap data" >&2
  exit 1
fi
echo "copied $db: $rows rows, integrity ok"
gzip -9 "$snap"
sha=$(sha256sum < "$snap.gz" | cut -d' ' -f1)
echo "compressed: $(stat -c %s "$snap.gz") bytes"

image=$(k3s kubectl -n gamemesh get cronjob postgres-backup \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')
if [[ $image != ghcr.io/alpnuhoglu/gamemesh-backup:* ]]; then
  echo "unexpected backup image: '$image'" >&2
  exit 1
fi
echo "image $image"

# The CronJob's pod settings (overlays/prod/backup.yaml), with standard input
# open for the copy.
spec=$(cat <<EOF
{"metadata": {"labels": {"app.kubernetes.io/name": "k3s-backup", "app.kubernetes.io/part-of": "gamemesh"}},
 "spec": {
  "restartPolicy": "Never", "activeDeadlineSeconds": 900, "automountServiceAccountToken": false,
  "securityContext": {"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
                      "seccompProfile": {"type": "RuntimeDefault"}},
  "containers": [{
   "name": "upload", "image": "$image", "stdin": true, "stdinOnce": true,
   "command": ["/usr/local/bin/upload", "-", "$name"],
   "envFrom": [{"secretRef": {"name": "gamemesh-k3s-backup"}}],
   "env": [
    {"name": "EXPECTED_SHA256", "value": "$sha"},
    {"name": "BACKUP_DEST", "value": "$dest"},
    {"name": "RCLONE_CONFIG_R2_TYPE", "value": "s3"},
    {"name": "RCLONE_CONFIG_R2_PROVIDER", "value": "Cloudflare"},
    {"name": "RCLONE_S3_NO_CHECK_BUCKET", "value": "true"},
    {"name": "RCLONE_CONFIG", "value": "/dev/null"},
    {"name": "HOME", "value": "/work"}],
   "securityContext": {"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
                       "capabilities": {"drop": ["ALL"]}},
   "volumeMounts": [{"name": "work", "mountPath": "/work"}],
   "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "500m", "memory": "256Mi"}}}],
  "volumes": [{"name": "work", "emptyDir": {"sizeLimit": "1Gi"}}]}}
EOF
)
k3s kubectl -n gamemesh run "$pod" -i --rm --quiet --restart=Never \
  --pod-running-timeout=5m --image="$image" --overrides="$spec" < "$snap.gz"
