#!/usr/bin/env bash
# Render the SVG logo kit to PNG with headless Chrome or Chromium, and write
# the favicons.
#
#   hack/artwork/render.sh artwork
#   CHROME=/usr/bin/chromium hack/artwork/render.sh artwork
set -euo pipefail

out=${1:-artwork}
chrome=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# render SVG PNG WIDTH HEIGHT: a transparent PNG of exactly WIDTH x HEIGHT.
render() {
  local abs
  abs="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
  printf '<!doctype html><html><body style="margin:0;background:transparent"><img src="file://%s" style="display:block;width:%spx;height:%spx"></body></html>' \
    "$abs" "$3" "$4" >"$tmp/page.html"
  "$chrome" --headless=new --disable-gpu --hide-scrollbars --allow-file-access-from-files \
    --default-background-color=00000000 --window-size="$3,$4" --screenshot="$2" "file://$tmp/page.html" >/dev/null 2>&1
}

# size SVG: the root element's width and height.
size() {
  grep -o 'width="[0-9.]*" height="[0-9.]*"' "$1" | head -1 | tr -dc '0-9 ' | awk '{print $1, $2}'
}

find "$out" -name '*.svg' -not -path '*/favicon/*' | sort | while read -r svg; do
  read -r w h < <(size "$svg")
  case "$svg" in
    */icon/* | */avatar/*) W=512 H=512 ;;
    */horizontal/*) H=256 W=$(((w * 256 + h / 2) / h)) ;;
    */stacked/*) W=512 H=$(((h * 512 + w / 2) / w)) ;;
    */social/*) W=1280 H=640 ;;
    *) continue ;;
  esac
  render "$svg" "${svg%.svg}.png" "$W" "$H"
  echo "${svg%.svg}.png ${W}x${H}"
done

mkdir -p "$out/favicon"
cp "$out/icon/color/kubeamber-icon-color.svg" "$out/favicon/favicon.svg"
render "$out/icon/color/kubeamber-icon-color.svg" "$out/favicon/favicon-32.png" 32 32
# iOS fills transparency with black, so the touch icon uses the avatar's light ground.
render "$out/avatar/kubeamber-avatar.svg" "$out/favicon/apple-touch-icon.png" 180 180
echo "$out/favicon/ (favicon.svg, favicon-32.png, apple-touch-icon.png)"
