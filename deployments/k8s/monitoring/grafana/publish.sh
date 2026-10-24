#!/bin/sh
# Keeps the status dashboard shared (phase 2, G6). Grafana stores a shared
# ("public") dashboard in its database and cannot provision one from a file,
# so this runs next to it (grafana.yaml, container "publisher") and every few
# minutes makes the share match what is written here:
#
#   - none yet                    -> create it with STATUS_TOKEN
#   - another token (made by hand) -> delete it, create ours
#   - off, or other settings      -> set them back
#
# The token is the status page's URL, public by design, and fixed so the URL
# survives a new volume. Settings: no time range change by the viewer, no
# annotations. Talks to Grafana on localhost only, without --compressed, so
# curl decodes no compressed response (.trivyignore.yaml, brotli). The admin
# password goes to curl on stdin (-K -), never on its command line.
set -u
: "${STATUS_TOKEN:?}" "${GF_SECURITY_ADMIN_PASSWORD:?}"
G=${GRAFANA_URL:-http://127.0.0.1:3000}
API="$G/api/dashboards/uid/${DASHBOARD_UID:-gamemesh-status}/public-dashboards"
WANT='{"isEnabled":true,"share":"public","timeSelectionEnabled":false,"annotationsEnabled":false}'
OUT=/tmp/publish.json

call() { # method url [body] -> HTTP code; body of the answer in $OUT
  printf 'user = "admin:%s"\n' "$GF_SECURITY_ADMIN_PASSWORD" |
    curl -s -K - -o "$OUT" -w '%{http_code}' -m 10 -X "$1" \
      -H 'Content-Type: application/json' ${3:+-d "$3"} "$2"
}

reconcile() {
  code=$(call GET "$API") || return 1
  case $code in
    404)
      code=$(call POST "$API" "${WANT%\}},\"accessToken\":\"$STATUS_TOKEN\"}")
      echo "created: $code"; [ "$code" = 200 ] ;;
    200)
      uid=$(sed -n 's/.*"uid":"\([^"]*\)".*/\1/p' "$OUT")
      if ! grep -q "\"accessToken\":\"$STATUS_TOKEN\"" "$OUT"; then
        echo "other token: deleting $uid ($(call DELETE "$API/$uid"))"
        reconcile; return
      fi
      for f in '"isEnabled":true' '"timeSelectionEnabled":false' '"annotationsEnabled":false'; do
        grep -q "$f" "$OUT" || { code=$(call PATCH "$API/$uid" "$WANT"); echo "settings reset: $code"; [ "$code" = 200 ]; return; }
      done ;;
    *) echo "Grafana answered $code"; return 1 ;;
  esac
}

while :; do
  if reconcile; then sleep "${INTERVAL:-300}"; else sleep 10; fi
done
