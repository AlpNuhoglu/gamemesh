#!/usr/bin/env bash
# Deploy one commit of the prod overlay to the cluster kubectl points at; if
# it does not come up, redeploy the commit that ran before (plan D3).
#
#   deploy.sh <commit sha>
#
# "The commit that ran before" is read from the cluster (the gateway's image
# tag) before anything changes, and redeployed with its own manifests, not
# only its images: a bad release can be a manifest change, and kubectl
# rollout undo would also leave the last-applied annotation stale, so the
# next apply would bring the bad change back. The exit status is non-zero
# whenever the requested commit is not what runs at the end.
#
# Needs: kubectl configured for the cluster (.github/actions/prod-cluster),
# git with the repository checked out in the working directory, curl, jq,
# python3, and SMOKE_PASSWORD in the environment (the smoke account's).
set -euo pipefail

# The images CI publishes for each commit (the services, and the backup job's
# since phase 2 E2).
SERVICES=(gateway player matchmaking leaderboard websocket outbox-relay presence backup)
API=https://api.gamemesh.app
# The WebSocket check (ws_smoke.py, beside this script) logs in as a fixed
# account that the owner creates by hand, once (docs/deployment.md). It never
# registers one: sign-up is not the smoke test's to check, and a broken
# sign-up must not fail a deploy. Its password is the production
# environment's secret SMOKE_PASSWORD; neither it nor the token is printed.
export SMOKE_API=$API SMOKE_WS=wss://api.gamemesh.app SMOKE_USER=benchsmoke
export SMOKE_PASSWORD=${SMOKE_PASSWORD:-}
WS_SMOKE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ws_smoke.py"
WORK=${RUNNER_TEMP:-$(mktemp -d)}/deploy
mkdir -p "$WORK"

# Every service image of a commit must exist before the cluster is touched.
images_exist() {
  local sha=$1 missing=0 token code
  for s in "${SERVICES[@]}"; do
    token=$(curl -fsS "https://ghcr.io/token?scope=repository:alpnuhoglu/gamemesh-${s}:pull" | jq -r .token)
    code=$(curl -s -o /dev/null -w '%{http_code}' -I -H "Authorization: Bearer ${token}" \
      -H 'Accept: application/vnd.oci.image.index.v1+json' \
      "https://ghcr.io/v2/alpnuhoglu/gamemesh-${s}/manifests/${sha}")
    if [ "$code" != 200 ]; then echo "missing image gamemesh-${s}:${sha} (HTTP ${code})"; missing=1; fi
  done
  return "$missing"
}

# The smoke account must work before the cluster is touched: without it the
# WebSocket check fails every deploy, and every deploy is rolled back. Only a
# refused login (401: the API answers that for a missing account and for a
# wrong password alike) stops here. Anything else (5xx, a timeout) can be
# production already unhealthy, which is when a deploy is wanted: a warning.
smoke_account_ok() {
  local rc=0
  if ! command -v python3 >/dev/null; then echo "::error::python3 is needed for the WebSocket check"; return 1; fi
  python3 "$WS_SMOKE" login || rc=$?
  case $rc in
    0) return 0 ;;
    3) echo "::error::smoke account missing or wrong password: create benchsmoke by hand and set SMOKE_PASSWORD (docs/deployment.md)"; return 1 ;;
    2) echo "::error::the WebSocket check is not configured (see above)"; return 1 ;;
    *) echo "::warning::could not log in as ${SMOKE_USER} before the deploy (see above); deploying anyway"; return 0 ;;
  esac
}

# A commit's own deployments/k8s, with the overlay pointed at its images.
# Prints the overlay directory.
prepare() {
  local sha=$1 tree="$WORK/tree-$1" f n m
  rm -rf "$tree"; mkdir -p "$tree"
  git cat-file -e "${sha}^{commit}" 2>/dev/null || git fetch --quiet --depth 1 origin "$sha" || return 1
  git archive "$sha" deployments/k8s | tar -x -C "$tree" || return 1
  f="$tree/deployments/k8s/overlays/prod/kustomization.yaml"
  # Counted from the commit's own overlay: an older commit, from before the
  # backup image, has one placeholder less, and a rollback to it must work.
  n=$(grep -c 'newTag: set-by-deploy' "$f" || true)
  if [ "$n" -lt 7 ]; then echo "expected at least 7 placeholders in ${sha}'s overlay, found $n" >&2; return 1; fi
  sed -i "s/newTag: set-by-deploy/newTag: ${sha}/" "$f"
  kubectl kustomize "$tree/deployments/k8s/overlays/prod" > "$WORK/render-$sha.yaml" || return 1
  if grep -q 'set-by-deploy' "$WORK/render-$sha.yaml"; then echo "a placeholder is left" >&2; return 1; fi
  # Distinct images, not lines: one image can run in more than one workload
  # (two CronJobs share the backup image), and it has one placeholder. Lines
  # would also match in an older overlay, where each image appears once, but
  # distinct references are what a placeholder stands for, in every commit.
  m=$( (grep -oE "image: ghcr.io/alpnuhoglu/gamemesh-[a-z-]+:${sha}$" "$WORK/render-$sha.yaml" || true) | sort -u | wc -l)
  m=$((m)) # wc pads the number with spaces on macOS
  if [ "$m" -ne "$n" ]; then echo "expected $n distinct images at ${sha}, found $m" >&2; return 1; fi
  echo "$tree/deployments/k8s/overlays/prod"
}

# Ready pods are not yet users reaching the commit: Traefik, TLS and DNS sit
# in between. /health must report the commit, and a proxied route (gateway,
# leaderboard, Redis) must answer, from outside, within two minutes. Then
# the WebSocket check (smoke_ws).
smoke() {
  local sha=$1 v code
  for _ in $(seq 1 24); do
    v=$(curl -fsS --max-time 10 "$API/health" | jq -r .version 2>/dev/null || true)
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$API/api/v1/leaderboard" || true)
    if [ "$v" = "$sha" ] && [ "$code" = 200 ]; then
      echo "$API serves ${sha}; /api/v1/leaderboard answers 200"
      if smoke_ws; then return 0; fi
      return 1
    fi
    echo "serving ${v:-nothing}, /api/v1/leaderboard ${code:-no answer}; waiting"; sleep 5
  done
  return 1
}

# /ws carries the game, and the HTTP checks never reach it: a release that
# breaks the upgrade or the read loop would pass them. ws_smoke.py logs in,
# opens /ws, joins a new room, needs an answer for that room within 5 s,
# closes and logs out; what counts as an answer is decided (and explained)
# there. Retried like the HTTP checks, since Traefik may still be switching
# backends. A try that fails fast takes well under a second; one that runs
# into every timeout, about a minute.
smoke_ws() {
  local i
  for i in 1 2 3 4 5; do
    if python3 "$WS_SMOKE" check; then return 0; fi
    if [ "$i" -lt 5 ]; then echo "WebSocket check failed (try $i of 5); waiting"; sleep 5; fi
  done
  return 1
}

# apply only means "accepted": every workload has to roll out and pass
# readiness. progressDeadlineSeconds (300) fails a stuck Deployment early.
deploy() {
  local sha=$1 dir w
  echo "::group::deploy ${sha}"
  dir=$(prepare "$sha") || { echo "::endgroup::"; return 1; }
  kubectl apply -k "$dir" --dry-run=server || { echo "::endgroup::"; return 1; }
  kubectl apply -k "$dir" || { echo "::endgroup::"; return 1; }
  for w in $(kubectl get -k "$dir" -o name | grep -E '^(deployment|statefulset)\.apps/'); do
    kubectl -n gamemesh rollout status "$w" --timeout=6m || { echo "::endgroup::"; return 1; }
  done
  smoke "$sha" || { echo "::endgroup::"; return 1; }
  echo "::endgroup::"
}

# Monitoring (phase 2, G4), from the same commit's tree, once the app runs.
# Separate from the app on purpose: if it fails the job is red, but the app
# is not rolled back, since users do not depend on it. A commit from before
# monitoring existed has nothing to apply.
deploy_monitoring() {
  local sha=$1 dir="$WORK/tree-$1/deployments/k8s/monitoring" w
  if [ ! -d "$dir" ]; then echo "no monitoring in ${sha}"; return 0; fi
  echo "::group::monitoring ${sha}"
  kubectl apply -k "$dir" --dry-run=server || { echo "::endgroup::"; return 1; }
  kubectl apply -k "$dir" || { echo "::endgroup::"; return 1; }
  for w in $(kubectl get -k "$dir" -o name | grep -E '^deployment\.apps/'); do
    kubectl -n monitoring rollout status "$w" --timeout=6m || { echo "::endgroup::"; return 1; }
  done
  echo "::endgroup::"
}

show_state() {
  echo "::group::cluster state"
  kubectl -n gamemesh get deploy,sts,pods -o wide || true
  kubectl -n gamemesh get events --sort-by=.lastTimestamp | tail -40 || true
  echo "::endgroup::"
}

main() {
  SHA=${1:?usage: deploy.sh <commit sha>}
  if ! [[ $SHA =~ ^[0-9a-f]{40}$ ]]; then
    echo "::error::not a full commit SHA: $SHA"; exit 1
  fi
  PREV=$(kubectl -n gamemesh get deploy gateway -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null \
    | sed -n 's#.*:\([0-9a-f]\{40\}\)$#\1#p' || true)
  echo "running before: ${PREV:-unknown}"
  curl -fsS --max-time 10 "$API/health" || echo "health check failed before the deploy"
  echo

  if [ -z "$SMOKE_PASSWORD" ]; then
    echo "::error::SMOKE_PASSWORD is not set: the production environment needs the benchsmoke account's password (docs/deployment.md); nothing was changed"; exit 1
  fi
  if ! images_exist "$SHA"; then
    echo "::error::images for ${SHA} are not published; nothing was changed"; exit 1
  fi
  if ! smoke_account_ok; then exit 1; fi

  if deploy "$SHA"; then
    echo "deployed ${SHA}"
    if deploy_monitoring "$SHA"; then exit 0; fi
    echo "::error::monitoring of ${SHA} failed; the app is deployed and stays"
    kubectl -n monitoring get deploy,pods -o wide || true
    kubectl -n monitoring get events --sort-by=.lastTimestamp | tail -20 || true
    exit 1
  fi

  echo "::error::deploy of ${SHA} failed"
  show_state
  if [ -z "$PREV" ] || [ "$PREV" = "$SHA" ]; then
    echo "::error::no earlier commit to return to"; exit 1
  fi
  echo "rolling back to ${PREV}"
  if deploy "$PREV"; then
    echo "::error::rolled back: ${PREV} runs again; ${SHA} is not deployed"
  else
    echo "::error::ROLLBACK to ${PREV} FAILED"
    show_state
  fi
  exit 1
}

# Run when executed; the functions can also be sourced on their own (tests).
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
