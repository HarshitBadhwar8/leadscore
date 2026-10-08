# shellcheck shell=sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Sourced by every examples/*/run.sh. It builds leadscore and moves into a
# temporary folder that stands in for a fresh clone, removed on exit. That
# folder holds the binary as ./leadscore (the Quick start's `go build -o
# leadscore`) and a copy of examples/, and the binary is also first on PATH as
# leadscore. Nothing an example runs can reach the network or the machine's
# own settings:
#
#   - The examples score CSV files into SQLite and need no account or key.
#   - HTTP_PROXY, HTTPS_PROXY and their lowercase forms point at a closed
#     loopback port, and NO_PROXY exempts only 127.0.0.1 and localhost, so a
#     request to any other host fails instead of leaving the machine.
#   - HOME is an empty temporary directory.
#
# The folder's name starts with leadscore-example., so scripts/run-examples.sh
# can write it as /home/you/leadscore in the output it compares.

repo=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/leadscore-example.XXXXXX")
cleanup() {
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

mkdir "$work/bin" "$work/home"
# Built before HOME moves, so go keeps using its own module and build caches.
(
	cd "$repo" || exit 1
	go build -o "$work/leadscore" ./cmd/leadscore
)
cp "$work/leadscore" "$work/bin/leadscore"
cp -R "$repo/examples" "$work/examples"

PATH=$work/bin:$PATH
HOME=$work/home
# An inherited NO_PROXY, such as NO_PROXY=*, would let requests bypass the
# closed proxy, so both forms are cleared and only loopback is exempt.
unset NO_PROXY no_proxy
HTTP_PROXY=http://127.0.0.1:9
HTTPS_PROXY=$HTTP_PROXY
http_proxy=$HTTP_PROXY
https_proxy=$HTTP_PROXY
NO_PROXY=127.0.0.1,localhost
export PATH HOME HTTP_PROXY HTTPS_PROXY http_proxy https_proxy NO_PROXY
cd "$work" || exit 1
