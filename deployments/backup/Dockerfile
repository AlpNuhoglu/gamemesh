# The Postgres backup job's image (phase 2, E2): pg_dump, age and rclone on
# Alpine, and backup.sh. Built, scanned and published by CI with the services
# (as gamemesh-backup), from this directory as its own build context.
#
# pg_dump's major version must match the server's (postgres:16 in the base):
# postgresql16-client. The packages are not pinned to a version: Alpine drops
# superseded ones from its index, so a pin would break the build at the next
# security update; each build takes the current patch, and the image scan
# checks it.
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache postgresql16-client age rclone
COPY backup.sh /usr/local/bin/backup
# Numeric, so Kubernetes' runAsNonRoot can check it.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/backup"]
