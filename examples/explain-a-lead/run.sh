#!/bin/sh
# Scores the made-up leads, then asks why one lead is on the call list and
# why another is on no list at all.
set -eu
# shellcheck source=scripts/example-setup.sh
. "$(dirname "$0")/../../scripts/example-setup.sh"

mkdir try
cp examples/leads.csv try/leads.csv
cp examples/rubric.csv-only.yml try/rubric.yml
cd try
cat > leadscore.yml <<'EOF'
version: 1
store: { type: sqlite, path: leadscore.db }
sources:
  - { id: leads, type: csv, path: leads.csv }
export: { dir: out }
EOF
leadscore run > /dev/null
leadscore explain anna.weber@kranlogistik.example
echo
leadscore explain tom.hale@ironbridge.example
