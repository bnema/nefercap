#!/bin/sh
# Regenerates neferwl.go (test-only server bindings of NeferWL's capture
# extension) from the vendored XML with the wlgen of the purego-libwayland
# module in use. Offline: go.work or the module cache.
set -eu
cd "$(dirname "$0")"
mod=$(go list -m -f '{{.Dir}}' github.com/bnema/purego-libwayland)
p=github.com/bnema/purego-libwayland/protocol
go run github.com/bnema/purego-libwayland/cmd/wlgen -package neferwl -out neferwl.go \
  -import wayland=$p/wayland -import-xml wayland="$mod/protocols/wayland.xml" \
  -import extimagecapturesource=$p/extimagecapturesource -import-xml extimagecapturesource="$mod/protocols/ext-image-capture-source-v1.xml" \
  -import extimagecopycapture=$p/extimagecopycapture -import-xml extimagecopycapture="$mod/protocols/ext-image-copy-capture-v1.xml" \
  -import extworkspace=$p/extworkspace -import-xml extworkspace="$mod/protocols/ext-workspace-v1.xml" \
  neferwl-image-capture-v1.xml
