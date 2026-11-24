#!/usr/bin/env bash
# Downloads the v2fly geoip.dat used by the Logs tab to label DNS answers with
# country flags. The file is intentionally not committed to git.
set -euo pipefail

dest="${1:-geoip.dat}"
url="${GEOIP_URL:-https://github.com/v2fly/geoip/releases/latest/download/geoip.dat}"

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi

echo "downloading geoip.dat -> ${dest}"
curl -fL --retry 3 -o "${dest}.tmp" "$url"
mv "${dest}.tmp" "$dest"
echo "saved $(du -h "$dest" | cut -f1) to ${dest}"
