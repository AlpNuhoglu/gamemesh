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
# git with the repository checked out in the working directory, curl, jq.
set -euo pipefail

SERVICES=(gateway player matchmaking leaderboard websocket outbox-relay presence)
API=https://api.gamemesh.app
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

# A commit's own deployments/k8s, with the overlay pointed at its images.
# Prints the overlay directory.
prepare() {
  local sha=$1 tree="$WORK/tree-$1" f n m
  rm -rf "$tree"; mkdir -p "$tree"
  git cat-file -e "${sha}^{commit}" 2>/dev/null || git fetch --quiet --depth 1 origin "$sha" || return 1
  git archive "$sha" deployments/k8s | tar -x -C "$tree" || return 1
  f="$tree/deployments/k8s/overlays/prod/kustomization.yaml"
  n=$(grep -c 'newTag: set-by-deploy' "$f" || true)
  if [ "$n" -ne 7 ]; then echo "expected 7 placeholders in ${sha}'s overlay, found $n" >&2; return 1; fi
  sed -i "s/newTag: set-by-deploy/newTag: ${sha}/" "$f"
  kubectl kustomize "$tree/deployments/k8s/overlays/prod" > "$WORK/render-$sha.yaml" || return 1
  if grep -q 'set-by-deploy' "$WORK/render-$sha.yaml"; then echo "a placeholder is left" >&2; return 1; fi
  m=$(grep -cE "image: ghcr.io/alpnuhoglu/gamemesh-[a-z-]+:${sha}$" "$WORK/render-$sha.yaml" || true)
  if [ "$m" -ne 7 ]; then echo "expected 7 images at ${sha}, found $m" >&2; return 1; fi
  echo "$tree/deployments/k8s/overlays/prod"
}

# Ready pods are not yet users reaching the commit: Traefik, TLS and DNS sit
# in between. /health must report the commit, and a proxied route (gateway,
# leaderboard, Redis) must answer, from outside, within two minutes.
smoke() {
  local sha=$1 v code
  for _ in $(seq 1 24); do
    v=$(curl -fsS --max-time 10 "$API/health" | jq -r .version 2>/dev/null || true)
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$API/api/v1/leaderboard" || true)
    if [ "$v" = "$sha" ] && [ "$code" = 200 ]; then
      echo "$API serves ${sha}; /api/v1/leaderboard answers 200"; return 0
    fi
    echo "serving ${v:-nothing}, /api/v1/leaderboard ${code:-no answer}; waiting"; sleep 5
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

  if ! images_exist "$SHA"; then
    echo "::error::images for ${SHA} are not published; nothing was changed"; exit 1
  fi

  if deploy "$SHA"; then
    echo "deployed ${SHA}"; exit 0
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
