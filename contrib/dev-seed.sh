#!/usr/bin/env bash
#
# Push a few images with real provenance labels and record deployments against
# them, so a local instance has something in it. Development only.
#
# It exists because an empty dashboard tells you nothing about whether the
# dashboard is any good, and hand-assembling an OCI push with curl each time is
# tedious enough that people skip it.
#
#   contrib/dev-seed.sh [registry-url] [admin-user] [admin-password]

set -euo pipefail

REGISTRY="${1:-http://127.0.0.1:5100}"
ADMIN_USER="${2:-admin}"
ADMIN_PASS="${3:-devpassword}"
MANTLE="${MANTLE:-./bin/mantle}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

export MANTLE_CLI_CONFIG="${MANTLE_CLI_CONFIG:-$WORK/cli.yaml}"

say() { printf '  %s\n' "$*"; }

say "logging in to $REGISTRY"
"$MANTLE" login "$REGISTRY" --username "$ADMIN_USER" --token "$ADMIN_PASS" >/dev/null

for org in acme library; do
  "$MANTLE" org create "$org" >/dev/null 2>&1 || true
done
say "organizations ready"

TOKEN=$("$MANTLE" token create dev-seed --org acme --namespace acme/ \
  --role contributor --json 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])')

# push_image <repo> <tag> <commit> <layer-content> -> prints the manifest digest
push_image() {
  local repo="$1" tag="$2" commit="$3" layer="$4"

  python3 - "$commit" "$repo" "$tag" > "$WORK/config.json" <<'PY'
import json, sys
commit, repo, tag = sys.argv[1], sys.argv[2], sys.argv[3]
labels = {"org.opencontainers.image.source": "https://github.com/" + repo,
          "org.opencontainers.image.version": tag}
if commit:
    labels["org.opencontainers.image.revision"] = commit
print(json.dumps({"architecture": "amd64", "os": "linux",
                  "created": "2026-08-14T10:00:00Z",
                  "config": {"Labels": labels},
                  "rootfs": {"type": "layers", "diff_ids": []}}))
PY
  printf '%s' "$layer" > "$WORK/layer.bin"

  local cfg_digest cfg_size lay_digest lay_size
  cfg_digest="sha256:$(shasum -a 256 "$WORK/config.json" | awk '{print $1}')"
  cfg_size=$(wc -c < "$WORK/config.json" | tr -d ' ')
  lay_digest="sha256:$(shasum -a 256 "$WORK/layer.bin" | awk '{print $1}')"
  lay_size=$(wc -c < "$WORK/layer.bin" | tr -d ' ')

  upload "$repo" "$WORK/config.json" "$cfg_digest"
  upload "$repo" "$WORK/layer.bin"   "$lay_digest"

  python3 - "$cfg_digest" "$cfg_size" "$lay_digest" "$lay_size" > "$WORK/manifest.json" <<'PY'
import json, sys
cd, cs, ld, ls = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
print(json.dumps({
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"mediaType": "application/vnd.oci.image.config.v1+json",
             "digest": cd, "size": cs},
  "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
              "digest": ld, "size": ls}]}))
PY

  curl -sf -u "mantle:$TOKEN" -X PUT --data-binary "@$WORK/manifest.json" \
    -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
    -o /dev/null "$REGISTRY/v2/$repo/manifests/$tag"
  shasum -a 256 "$WORK/manifest.json" | awk '{print "sha256:"$1}'
}

# upload <repo> <file> <digest>
upload() {
  local repo="$1" file="$2" digest="$3" location
  location=$(curl -sf -u "mantle:$TOKEN" -X POST -D - -o /dev/null \
    "$REGISTRY/v2/$repo/blobs/uploads/" | grep -i '^location:' | tr -d '\r' | awk '{print $2}')
  curl -sf -u "mantle:$TOKEN" -X PUT --data-binary "@$file" \
    -H 'Content-Type: application/octet-stream' \
    -o /dev/null "$REGISTRY$location?digest=$digest"
}

say "pushing images"
WEB_OLD=$(push_image acme/web v2.4.0 8e9d114c3f2a1b0d9e8f7a6b5c4d3e2f1a0b9c8d "web layer v2.4.0")
WEB_NEW=$(push_image acme/web v2.4.1 a3f81c2e9b7d4f0a1c2e3b4d5f6a7b8c9d0e1f2a "web layer v2.4.1")
API=$(push_image     acme/api v1.9.2 55a2f78b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f "api layer")
# No provenance labels: exercises the tag-shape inference path, which is
# recorded as a guess rather than a fact.
push_image acme/worker sha-c02e5d1 "" "worker layer" >/dev/null

say "recording deployments"
record() {
  "$MANTLE" deploy record --repo "$1" --digest "$2" --env "$3" \
    --performer "$4" --tool "$5" --deploy-id "$6" "${@:7}" >/dev/null
}
record acme/web "$WEB_OLD" production nima    ansible seed-web-1 --host web-1 --host web-2
record acme/web "$WEB_NEW" production nima    ansible seed-web-2 --host web-1 --host web-2
record acme/web "$WEB_NEW" staging    ci      compose seed-web-3 --host stg-1
record acme/api "$API"     production ci      compose seed-api-1 --host api-1

say "done"
"$MANTLE" repo list
