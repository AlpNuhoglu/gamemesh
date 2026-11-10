#!/bin/sh
# shellcheck shell=busybox
# (Alpine's busybox sh, which has pipefail)
# Daily Postgres backup (phase 2, E2): dump, encrypt, upload, report.
#
#   pg_dump --format=custom | age -r <the admin's public key> | rclone -> R2
#
# Only the admin's age PUBLIC key is here: the server can write a backup but
# never read one, so a compromised server does not expose the old ones. The
# private key is offline, with the admin (docs/deployment.md, "Backups").
#
# Each run writes a new object, named by the time it started; the bucket's
# lock keeps every object undeletable and unchangeable for 30 days, and a
# lifecycle rule removes it after 90. When PING_URL is set (the dead man's
# switch, phase 2 F2), the run pings it on success and its /fail address on
# failure; a run that never runs, or is killed, pings nothing, and the switch
# raises the alarm when the ping is overdue.
#
# Environment:
#   PGHOST PGUSER PGPASSWORD PGDATABASE  the database (libpq's own names)
#   AGE_RECIPIENT                        the admin's public key, age1...
#   BACKUP_DEST                          rclone destination, e.g. r2:gamemesh-backups/postgres
#   RCLONE_CONFIG_R2_*                   the remote (type, provider, endpoint, keys)
#   PING_URL                             optional
set -eu
set -o pipefail

# The dead man's switch hears how the run ended (phase 2, E4): a plain ping
# on success, /fail on any other exit, so "ran and failed" and "never ran"
# are different alerts. Nothing about the failure is sent; the reason is in
# this log. A ping that cannot be sent leaves the run's own result as it is.
report() {
  status=$?
  trap - EXIT
  if [ -n "${PING_URL:-}" ]; then
    if [ "$status" -eq 0 ]; then
      wget -q -T 10 -O /dev/null "$PING_URL" && echo "pinged the dead man's switch" ||
        echo "could not ping the dead man's switch" >&2
    else
      wget -q -T 10 -O /dev/null "$PING_URL/fail" &&
        echo "reported the failure to the dead man's switch" >&2 ||
        echo "could not report the failure to the dead man's switch" >&2
    fi
  fi
  exit "$status"
}
trap report EXIT

: "${PGHOST:?}" "${PGUSER:?}" "${PGPASSWORD:?}" "${PGDATABASE:?}" "${AGE_RECIPIENT:?}" "${BACKUP_DEST:?}"

stamp=$(date -u +%Y-%m-%dT%H%M%SZ)
name="$PGDATABASE-$stamp.dump.age"
work=${WORK_DIR:-/work}
out="$work/$name"

echo "dumping $PGDATABASE from $PGHOST"
pg_dump --format=custom --no-owner --no-privileges | age --encrypt --recipient "$AGE_RECIPIENT" > "$out"
size=$(wc -c < "$out")
# An empty database still dumps to a few kilobytes; much less means pg_dump
# wrote nothing the pipe noticed.
if [ "$size" -lt 1024 ]; then
  echo "backup is only $size bytes; not uploading" >&2
  exit 1
fi
echo "encrypted dump: $size bytes"

rclone copyto "$out" "$BACKUP_DEST/$name"
# Compare what arrived with what was sent.
remote=$(rclone size --json "$BACKUP_DEST/$name" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p')
if [ "$remote" != "$size" ]; then
  echo "uploaded $remote bytes, expected $size" >&2
  exit 1
fi
echo "uploaded $BACKUP_DEST/$name"
