#!/usr/bin/env bash
# Proves the registry loses weight: an old image is removed, the current one is
# not, and the store on the volume is measurably smaller afterwards.
#
# Four things are asserted separately because each of them can be true while
# the collector is still broken:
#
#   1. The pruned tag stops resolving. Cheap, and on its own it proves nothing:
#      unlinking a tag frees no space whatever — the manifest it pointed at and
#      every blob that manifest names are all still there.
#   2. A blob only the pruned image referenced can no longer be fetched. This is
#      the assertion that says the sweep ran and reached the blob store, rather
#      than the tag directory and no further.
#   3. The newest image still resolves, manifest *and* layer. A collector that
#      deletes everything passes 1 and 2.
#   4. The store shrank, read off the volume before and after — not taken from
#      the job's own report. A job that prints two numbers and collects nothing
#      is precisely the failure the first three assertions can be fooled by.
#   5. The build cache survived the sweep that pruned the images, and did not
#      spend one of the retained slots. A collector that counts it as an image
#      keeps nine builds where the manifest says ten.
#   6. A cache last written long ago is collected. Backdated on the volume, so
#      that "nothing was collected" and "there was nothing to collect" cannot
#      produce the same green.
#
# The images are pushed with curl rather than built. What is under test is the
# store, and to a registry a layer is bytes with a digest; building one would
# add a builder to the list of things that can fail this.
set -uo pipefail

NAMESPACE="${NAMESPACE:-damga-registry}"
PORT="${PORT:-15000}"
REPO="${REPO:-gc-probe/app}"
# Big enough that the freed space is unmistakable next to an empty store's few
# KiB, small enough that three of them push in seconds.
LAYER_MB="${LAYER_MB:-4}"
BASE="http://127.0.0.1:${PORT}"
JOB="registry-gc-probe-$$"
FAILED=0
PF_PID=""
TMP="$(mktemp -d)"

pass() { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; FAILED=1; }
note() { printf '  ....  %s\n' "$1"; }

cleanup() {
  [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null
  kubectl -n "$NAMESPACE" delete job "$JOB" --ignore-not-found --wait=false >/dev/null 2>&1
  rm -rf "$TMP"
}
trap cleanup EXIT

kubectl -n "$NAMESPACE" get cronjob registry-gc >/dev/null 2>&1 || {
  echo "no registry-gc CronJob in '$NAMESPACE' — nothing deletes an image here" >&2
  echo "run: kubectl apply -f cluster/registry.yaml" >&2
  exit 1
}
kubectl -n "$NAMESPACE" rollout status deployment/registry --timeout=120s >/dev/null || {
  echo "the registry is not serving; a store that is not up cannot be swept" >&2
  exit 1
}

# du is read inside the registry's own container. It needs `kubectl exec`, which
# needs the kubelet's serving certificate to be signed — scripts/bootstrap.sh
# and CI both approve those, and on a cluster where they were not this fails
# here rather than three assertions later.
store_kib() {
  kubectl -n "$NAMESPACE" exec deploy/registry -- du -sk /var/lib/registry 2>/dev/null | cut -f1
}
[[ -n "$(store_kib)" ]] || {
  echo "could not measure the store: kubectl exec into the registry failed" >&2
  echo "on a cluster built by hand: ./scripts/approve-kubelet-certs.sh" >&2
  exit 1
}

kubectl -n "$NAMESPACE" port-forward svc/registry "${PORT}:5000" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/v2/" 2>/dev/null)" == 200 ]] && break
  sleep 1
done
[[ "$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/v2/" 2>/dev/null)" == 200 ]] || {
  echo "the registry API never answered on $BASE/v2/" >&2
  exit 1
}

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# push_blob <file> -> digest. Two requests, as the API requires: one to open an
# upload and one to close it with the digest the registry then verifies.
push_blob() {
  local file="$1" digest location code
  digest="sha256:$(sha "$file")"
  location=$(curl -sS -X POST -D - -o /dev/null "$BASE/v2/$REPO/blobs/uploads/" |
             tr -d '\r' | awk 'tolower($1) == "location:" { print $2 }')
  # The Location comes back either absolute or as a path, and already carries
  # the upload state in a query string.
  case "$location" in /*) location="$BASE$location" ;; esac
  case "$location" in *\?*) location="$location&digest=$digest" ;;
                       *)   location="$location?digest=$digest" ;; esac
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
              -H 'Content-Type: application/octet-stream' \
              --data-binary "@$file" "$location")
  [[ "$code" == 201 ]] || { echo "blob upload answered $code" >&2; return 1; }
  printf '%s' "$digest"
}

# push_image <tag> -> the digest of the layer only this image references.
push_image() {
  local tag="$1" layer="$TMP/layer" config="$TMP/config" manifest="$TMP/manifest"
  local layer_digest config_digest code
  dd if=/dev/urandom of="$layer" bs=1048576 count="$LAYER_MB" status=none
  layer_digest=$(push_blob "$layer") || return 1
  printf '{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["%s"]}}' \
    "$layer_digest" > "$config"
  config_digest=$(push_blob "$config") || return 1
  cat > "$manifest" <<JSON
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
 "config":{"mediaType":"application/vnd.oci.image.config.v1+json",
           "digest":"$config_digest","size":$(wc -c <"$config" | tr -d ' ')},
 "layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",
            "digest":"$layer_digest","size":$(wc -c <"$layer" | tr -d ' ')}]}
JSON
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
              -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
              --data-binary "@$manifest" "$BASE/v2/$REPO/manifests/$tag")
  [[ "$code" == 201 ]] || { echo "manifest PUT answered $code" >&2; return 1; }
  printf '%s' "$layer_digest"
}

manifest_code() {
  curl -sS -o /dev/null -w '%{http_code}' \
       -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
       "$BASE/v2/$REPO/manifests/$1"
}

# A blob is asserted by downloading it, not by its status code — and that is a
# measurement rather than a preference. A registry that is still running answers
# 200 for a blob the sweep deleted underneath it: the descriptor is in the
# in-memory cache the shipped configuration turns on, so the headers are written
# before anything opens the file, and the transfer then dies. Asked for a
# collected 4MiB layer, curl reported `transfer closed with 4194304 bytes
# remaining to read` — with a 200. `-f` plus a complete body is the only form of
# this question with an answer worth having.
blob_downloads() { curl -sS -f -o /dev/null "$BASE/v2/$REPO/blobs/$1" 2>/dev/null; }

# Tags are commit SHAs in production and carry no order, so the job sorts by the
# mtime the store wrote when it accepted the push — which has one-second
# resolution. Three pushes inside the same second tie, and the tie decides which
# image survives, so this waits.
echo "pushing three ${LAYER_MB}MiB images to $REPO"
declare -a TAGS=(aaaaaaa1 bbbbbbb2 ccccccc3)
declare -a LAYERS=()
for tag in "${TAGS[@]}"; do
  layer=$(push_image "$tag") || { echo "could not push $tag" >&2; exit 1; }
  LAYERS+=("$layer")
  note "$tag -> ${layer:0:19}…"
  sleep 1
done

# The build cache, pushed as the builder pushes it: a tag in the same repository
# as the images built from it. internal/controller/build_job.go writes this name
# on every build, and the collector has to treat it as neither an image nor
# rubbish — see the two assertions about it below.
CACHE_TAG="${CACHE_TAG:-buildcache}"
CACHE_LAYER=$(push_image "$CACHE_TAG") || { echo "could not push the cache tag" >&2; exit 1; }
note "$CACHE_TAG -> ${CACHE_LAYER:0:19}…"

BEFORE=$(store_kib)
note "store before: ${BEFORE} KiB"

# The CronJob keeps ten tags. Pushing eleven images would exercise the same
# mechanism eleven times more slowly, so the run overrides the count and nothing
# else: same image, same command, same volume.
echo "running the collector with RETAIN=1"
kubectl -n "$NAMESPACE" create job "$JOB" --from=cronjob/registry-gc \
        --dry-run=client -o json |
  jq '(.spec.template.spec.containers[0].env[] | select(.name == "RETAIN") | .value) = "1"' |
  kubectl -n "$NAMESPACE" apply -f - >/dev/null || {
    echo "could not create the collector job" >&2
    exit 1
  }

for _ in $(seq 1 60); do
  succeeded=$(kubectl -n "$NAMESPACE" get job "$JOB" -o jsonpath='{.status.succeeded}' 2>/dev/null)
  failed=$(kubectl -n "$NAMESPACE" get job "$JOB" -o jsonpath='{.status.failed}' 2>/dev/null)
  [[ "${succeeded:-0}" -ge 1 || "${failed:-0}" -ge 1 ]] && break
  sleep 5
done
kubectl -n "$NAMESPACE" logs "job/$JOB" --tail=-1 2>/dev/null | sed 's/^/  | /'
if [[ "${succeeded:-0}" -lt 1 ]]; then
  # A Pending pod here is usually the podAffinity: the job has to land on the
  # registry's node to mount a ReadWriteOnce claim, and it says so nowhere else.
  kubectl -n "$NAMESPACE" describe job "$JOB" | tail -20
  echo "::error::the collector did not finish; nothing was collected" >&2
  exit 1
fi

AFTER=$(store_kib)

# 1. the pruned tags
for tag in "${TAGS[0]}" "${TAGS[1]}"; do
  code=$(manifest_code "$tag")
  [[ "$code" == 404 ]] && pass "$tag was pruned (manifest $code)" \
                       || fail "$tag still resolves ($code) — the retention count was not applied"
done

# 2. the blob only a pruned image referenced
blob_downloads "${LAYERS[0]}" \
  && fail "the pruned image's layer still downloads in full — the tag was unlinked and nothing was \
swept, which frees no space at all" \
  || pass "the pruned image's layer no longer downloads"

# 3. the image that was kept
code=$(manifest_code "${TAGS[2]}")
[[ "$code" == 200 ]] && pass "${TAGS[2]} still resolves ($code)" \
                     || fail "${TAGS[2]} answers $code — the collector took the newest image"
blob_downloads "${LAYERS[2]}" \
  && pass "the kept image's layer downloads in full" \
  || fail "the kept image's layer did not come back whole — its manifest survived and its bytes did \
not, which is an image that pulls until the layer is asked for"

# 4. the size, on the volume
FREED=$(( BEFORE - AFTER ))
MIN=$(( LAYER_MB * 1024 * 2 ))
note "store after: ${AFTER} KiB (freed ${FREED} KiB, two ${LAYER_MB}MiB layers = ${MIN} KiB)"
[[ "$FREED" -ge "$MIN" ]] && pass "the store shrank by ${FREED} KiB" \
                          || fail "the store gave back ${FREED} KiB and the two pruned images hold at \
least ${MIN} KiB — the manifests went and the bytes stayed"

# 5. the cache is not an image, and did not spend an image's slot
#
# RETAIN was 1 for that run. Four tags were pushed and the newest image is the
# one that had to survive; if the collector counted the cache as a tag, the
# cache is the newest thing in the repository and the image before it goes
# instead — which is nine builds kept where the manifest says ten, and nobody
# would see it until a rollback found nothing.
code=$(manifest_code "$CACHE_TAG")
[[ "$code" == 200 ]] && pass "the fresh cache survived (manifest $code)" \
                     || fail "the cache answers $code — a cache that is collected on the next \
sweep is a cache that is rebuilt on every build, which is the opposite of what it is for"

# 6. a cache nothing has refreshed is collected
#
# Backdated on the volume rather than waited for, because the alternative is a
# test that takes a fortnight. The link's mtime is what the job reads, and it is
# what the store writes when it accepts a push — so moving it is the same input
# an abandoned repository produces, arrived at sooner.
CACHE_LINK="/var/lib/registry/docker/registry/v2/repositories/$REPO/_manifests/tags/$CACHE_TAG/current/link"
if kubectl -n "$NAMESPACE" exec deploy/registry -- touch -t 202001010000 "$CACHE_LINK" 2>/dev/null; then
  JOB2="${JOB}-stale"
  kubectl -n "$NAMESPACE" create job "$JOB2" --from=cronjob/registry-gc \
          --dry-run=client -o json |
    jq '(.spec.template.spec.containers[0].env[] | select(.name == "RETAIN") | .value) = "1"' |
    kubectl -n "$NAMESPACE" apply -f - >/dev/null || {
      echo "could not create the second collector job" >&2
      exit 1
    }
  for _ in $(seq 1 60); do
    s2=$(kubectl -n "$NAMESPACE" get job "$JOB2" -o jsonpath='{.status.succeeded}' 2>/dev/null)
    f2=$(kubectl -n "$NAMESPACE" get job "$JOB2" -o jsonpath='{.status.failed}' 2>/dev/null)
    [[ "${s2:-0}" -ge 1 || "${f2:-0}" -ge 1 ]] && break
    sleep 5
  done
  kubectl -n "$NAMESPACE" logs "job/$JOB2" --tail=-1 2>/dev/null | sed 's/^/  | /'
  kubectl -n "$NAMESPACE" delete job "$JOB2" --ignore-not-found --wait=false >/dev/null 2>&1

  code=$(manifest_code "$CACHE_TAG")
  [[ "$code" == 404 ]] && pass "a cache last written in 2020 was collected (manifest $code)" \
                       || fail "the stale cache answers $code — a repository nobody builds any \
more keeps its cache for ever, and nothing in the store says why it is growing"
else
  fail "could not backdate the cache link; the stale-cache rule was not exercised at all"
fi

# The kept image is left behind on purpose only if this cannot remove it: the
# repository is a probe's, not a tenant's, and it would otherwise sit in the
# store until it aged out of the retention window months from now.
for i in 0 1 2; do
  digest=$(curl -sS -D - -o /dev/null \
                -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
                "$BASE/v2/$REPO/manifests/${TAGS[$i]}" 2>/dev/null |
           tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }')
  [[ -n "$digest" ]] && curl -sS -o /dev/null -X DELETE "$BASE/v2/$REPO/manifests/$digest"
done
note "probe manifests deleted; their blobs go on the next scheduled sweep"

exit "$FAILED"
