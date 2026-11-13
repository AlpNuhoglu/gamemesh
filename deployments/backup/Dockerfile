# The Postgres backup job's image (phase 2, E2): pg_dump, age and rclone on
# Alpine, and backup.sh. Built, scanned and published by CI with the services
# (as gamemesh-backup), from this directory as its own build context. The
# server's k3s backup (E3) runs the same image for upload.sh, in a pod, so
# the host needs no age or rclone of its own (Ubuntu's are built with
# go1.22.2 and never rebuilt).
#
# pg_dump's major version must match the server's (postgres:16 in the base):
# postgresql16-client. The packages are not pinned to a version: Alpine drops
# superseded ones from its index, so a pin would break the build at the next
# security update; each build takes the current patch, and the image scan
# checks it.
#
# The client has a floor instead, raised when a fix has to be taken: the
# build cache keys on this line, so without a change here a cached layer
# keeps the old package however new the index is (16.15 stayed after
# CVE-2026-116304 was fixed in 16.16). A floor, not a pin, still builds when
# Alpine moves on.
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache 'postgresql16-client>=16.16' age rclone
COPY backup.sh /usr/local/bin/backup
COPY upload.sh /usr/local/bin/upload
# Numeric, so Kubernetes' runAsNonRoot can check it.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/backup"]
