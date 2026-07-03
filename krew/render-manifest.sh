#!/usr/bin/env bash
# Render the krew plugin manifest from krew/kconmon.yaml.tmpl by substituting the
# release version and the sha256 of each platform archive built by goreleaser.
#
# Usage: krew/render-manifest.sh <version> <dist-dir> <output-file>
#   version:    release version WITHOUT the leading "v" (e.g. 1.3.0)
#   dist-dir:   goreleaser output dir containing kconmon_<version>_<os>_<arch>.tar.gz
#   output:     path to write the rendered manifest (e.g. kconmon.yaml)
set -euo pipefail

VERSION="${1:?version required (no leading v)}"
DIST="${2:?dist dir required}"
OUT="${3:?output file required}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMPL="${SCRIPT_DIR}/kconmon.yaml.tmpl"

# sha256 of an archive, portable across sha256sum (Linux) and shasum (macOS).
sum() {
  local f="${DIST}/kconmon_${VERSION}_${1}.tar.gz"
  if [ ! -f "$f" ]; then
    echo "missing archive: $f" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$f" | awk '{print $1}'
  else
    shasum -a 256 "$f" | awk '{print $1}'
  fi
}

sed \
  -e "s/{{VERSION}}/${VERSION}/g" \
  -e "s/{{SHA256_LINUX_AMD64}}/$(sum linux_amd64)/g" \
  -e "s/{{SHA256_LINUX_ARM64}}/$(sum linux_arm64)/g" \
  -e "s/{{SHA256_DARWIN_AMD64}}/$(sum darwin_amd64)/g" \
  -e "s/{{SHA256_DARWIN_ARM64}}/$(sum darwin_arm64)/g" \
  "$TMPL" > "$OUT"

echo "rendered $OUT for v${VERSION}"
