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
# go-licenses files golang.org/x/sys under its packages (unix, windows) on
# some machines and under the module on others. Keep one copy, at the module.
sys="$out/merged/golang.org/x/sys"
for d in "$sys"/*/; do
  if [ -f "$d/LICENSE" ]; then
    mv "$d/LICENSE" "$sys/LICENSE"
    rmdir "$d" 2>/dev/null || true
  fi
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
  # A version bump can leave the license text unchanged, so NOTICE's
  # versions are checked too: every module compiled in, at its version.
  stale=0
  for m in $(for os in linux darwin windows; do
    GOOS=$os go list -deps -f '{{with .Module}}{{.Path}}@{{.Version}}{{end}}' ./cmd/leadscore
  done | sort -u); do
    mod=${m%@*} ver=${m#*@}
    [ "$mod" = github.com/HarshitBadhwar8/leadscore ] && continue
    if ! awk -v m="$mod" -v v="$ver," '$1 == "-" && ($2 == m || index($2, m "/") == 1) && $3 == v { found = 1 } END { exit !found }' NOTICE; then
      echo "NOTICE lacks $mod $ver" >&2
      stale=1
    fi
  done
  [ "$stale" = 0 ] || exit 1
  echo "third_party/licenses and NOTICE are up to date"
  exit 0
fi
rm -rf third_party/licenses
mkdir -p third_party
cp -R "$out/merged" third_party/licenses
echo "wrote third_party/licenses ($(find third_party/licenses -type f | wc -l | tr -d ' ') files)"
