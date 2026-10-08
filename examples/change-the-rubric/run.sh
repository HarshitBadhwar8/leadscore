#!/bin/sh
# Scores the made-up leads, then makes a warm path in worth 30 points instead
# of 10, checks the edited rubric, runs again, and shows the new top four.
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
leadscore ranked --csv | cut -d, -f4,10 | head -5
sed -i.bak 's/present: true }, points: 10 }/present: true }, points: 30 }/' rubric.yml
leadscore rules check rubric.yml
leadscore run > /dev/null
leadscore ranked --csv | cut -d, -f4,10 | head -5
