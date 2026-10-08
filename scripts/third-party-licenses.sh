#!/bin/sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
# Regenerates third_party/licenses: the license texts of every module compiled
# into the release binaries, for each release OS, plus the Go toolchain's own.
# With --check, it regenerates into a temp dir and fails if the committed folder
# differs. Needs go-licenses (go install github.com/google/go-licenses/v2@v2.0.1).
set -eu
cd "$(dirname "$0")/.."
check=0
[ "${1:-}" = "--check" ] && check=1
out=$(mktemp -d)
trap 'chmod -R u+w "$out" 2>/dev/null; rm -rf "$out"' EXIT
GOROOT=$(go env GOROOT)
export GOROOT
mkdir -p "$out/merged"
for os in linux darwin windows; do
  GOOS=$os go-licenses save ./cmd/leadscore --ignore github.com/HarshitBadhwar8/leadscore \
    --save_path="$out/$os" --force 2>/dev/null
  chmod -R u+w "$out/$os" "$out/merged"
  cp -R "$out/$os/." "$out/merged/"
done
mkdir -p "$out/merged/go"
# Some packaged toolchains keep LICENSE beside GOROOT rather than in it.
for f in "$GOROOT/LICENSE" "$GOROOT/../LICENSE"; do
  if [ -f "$f" ]; then cp "$f" "$out/merged/go/LICENSE"; break; fi
done
[ -f "$out/merged/go/LICENSE" ] || { echo "no Go LICENSE under $GOROOT" >&2; exit 1; }
chmod -R u+w "$out/merged"
if [ "$check" = 1 ]; then
  if ! diff -r "$out/merged" third_party/licenses >/dev/null; then
    echo "third_party/licenses is stale; run scripts/third-party-licenses.sh" >&2
    diff -r "$out/merged" third_party/licenses | head -20 >&2
    exit 1
  fi
  echo "third_party/licenses is up to date"
  exit 0
fi
rm -rf third_party/licenses
mkdir -p third_party
cp -R "$out/merged" third_party/licenses
echo "wrote third_party/licenses ($(find third_party/licenses -type f | wc -l | tr -d ' ') files)"
