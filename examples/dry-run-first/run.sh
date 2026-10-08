#!/bin/sh
# Previews a first run with --dry-run and shows it wrote nothing, then does
# the real run with pushes off, and previews again to show nothing would
# change.
set -eu
# shellcheck source=scripts/example-setup.sh
. "$(dirname "$0")/../../scripts/example-setup.sh"

mkdir -p try
cp examples/leads.csv try/leads.csv
cp examples/rubric.csv-only.yml try/rubric.yml
cd try
cat > leadscore.yml <<'EOF'
version: 1
store: { type: sqlite, path: leadscore.db }
sources:
  - { id: leads, type: csv, path: leads.csv }
export: { dir: out }
pushes_enabled: false
EOF
leadscore run --dry-run
ls
leadscore run
ls out
leadscore run --dry-run
