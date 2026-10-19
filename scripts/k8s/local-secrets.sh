#!/bin/sh
# Writes the local k3d overlay's secrets to deployments/k8s/overlays/local/.env.local
# (gitignored), generating each missing value with openssl rand -hex 32. Keys
# already in the file are kept, so a value only changes when the line is
# deleted. Never prints a value.
#
# Changing POSTGRES_PASSWORD on a cluster that already has a Postgres volume
# does not change the database's password (it is set once, at initdb): delete
# the postgres-data PVC or ALTER USER as well.
set -eu
file=deployments/k8s/overlays/local/.env.local
umask 077
touch "$file"
add() { # key value
  grep -q "^$1=" "$file" || { printf '%s=%s\n' "$1" "$2" >> "$file"; echo "generated $1"; }
}
add POSTGRES_USER gamemesh
add POSTGRES_DB gamemesh
for key in JWT_SECRET POSTGRES_PASSWORD REDIS_PASSWORD; do
  add "$key" "$(openssl rand -hex 32)"
done
