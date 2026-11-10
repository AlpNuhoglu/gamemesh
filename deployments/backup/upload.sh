#!/bin/sh
# shellcheck shell=busybox
# (Alpine's busybox sh, which has pipefail)
# Encrypt a file the host prepared and upload it (phase 2, E3): the k3s
# datastore copy that deployments/server/backup/k3s-backup.sh takes on the
# server and pipes into a pod of this image. The same steps as backup.sh
# after its dump:
#
#   age -r <the admin's public key> < file | rclone -> R2
#
# Usage: upload <file|-> <object name>; "-" reads standard input. The object
# gets ".age" appended.
#
# Environment: AGE_RECIPIENT, BACKUP_DEST, RCLONE_CONFIG_R2_*, PING_URL
# (optional; pinged on success, its /fail address on failure), as for
# backup.sh; EXPECTED_SHA256, when set, must match what arrived, so a copy
# cut short on its way is not uploaded.
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

: "${AGE_RECIPIENT:?}" "${BACKUP_DEST:?}"
[ $# -eq 2 ] || { echo "usage: upload <file|-> <object name>" >&2; exit 2; }
in=$1
name="$2.age"
work=${WORK_DIR:-/work}
out="$work/$name"

if [ "$in" = - ]; then
  in="$work/in"
  cat > "$in"
fi
if [ -n "${EXPECTED_SHA256:-}" ]; then
  got=$(sha256sum < "$in" | cut -d' ' -f1)
  if [ "$got" != "$EXPECTED_SHA256" ]; then
    echo "received $(wc -c < "$in") bytes whose sha256 is $got, expected $EXPECTED_SHA256" >&2
    exit 1
  fi
  echo "received $(wc -c < "$in") bytes, sha256 as expected"
fi

age --encrypt --recipient "$AGE_RECIPIENT" < "$in" > "$out"
size=$(wc -c < "$out")
# The host refuses an empty or damaged copy before this; this catches a
# file that was not there to read.
if [ "$size" -lt 1024 ]; then
  echo "encrypted file is only $size bytes; not uploading" >&2
  exit 1
fi
echo "encrypted: $size bytes"

rclone copyto "$out" "$BACKUP_DEST/$name"
remote=$(rclone size --json "$BACKUP_DEST/$name" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p')
if [ "$remote" != "$size" ]; then
  echo "uploaded $remote bytes, expected $size" >&2
  exit 1
fi
echo "uploaded $BACKUP_DEST/$name"
