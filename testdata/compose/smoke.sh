#!/usr/bin/env bash
# The Docker check CI runs (RFC 8.4): `docker compose up` with the repo's
# compose.yaml and a built image, then a timer run end to end: the first run
# scores the sample CSV, a webhook posted to the receiver is stored, the next
# timer run turns it into a lead, the health check turns healthy, and SIGTERM
# stops the container cleanly.
#
# Usage, from the repo root: testdata/compose/smoke.sh <image>
set -euo pipefail

image=${1:?usage: smoke.sh <image>}
repo=$(pwd)
dir=$(mktemp -d)
# mktemp makes the folder owner-only; a team's folder is normally readable by
# others, which the container's own user (10001) needs to read /config.
chmod 755 "$dir"
secret=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')

cp compose.yaml "$dir/"
cp testdata/compose/leadscore.yml testdata/compose/rubric.yml examples/leads.csv "$dir/"
mkdir "$dir/out"
chmod 700 "$dir/out"
sudo chown 10001:10001 "$dir/out"
printf 'LEADSCORE_IMAGE=%s\nLEADSCORE_RECEIVER_SECRET=%s\n' "$image" "$secret" > "$dir/.env"
cd "$dir"

dc() { docker compose "$@"; }
ls_() { dc exec -T leadscore leadscore "$@"; }
fail() {
  echo "FAIL: $*"
  dc ps || true
  dc logs --no-color leadscore || true
  dc down -v || true
  exit 1
}
# wait_for <seconds> <description> <command...>
wait_for() {
  local secs=$1 what=$2
  shift 2
  for _ in $(seq 1 "$secs"); do
    if "$@" >/dev/null 2>&1; then
      echo "ok: $what"
      return 0
    fi
    sleep 1
  done
  fail "timed out waiting for: $what"
}

dc up -d
cid=$(dc ps -q leadscore)

# The first run starts with serve and scores the sample CSV.
wait_for 120 "the first timer run succeeded" sh -c "docker compose exec -T leadscore leadscore status | grep -q last_success_at"
ls_ status
ls_ ranked | grep -q 'anna.weber@kranlogistik.example' || fail "the first run did not rank the sample leads"

# A wrong secret is refused; a golden visit body with the secret is stored.
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-Leadscore-Secret: wrong' \
  --data-binary @"$repo/testdata/events/apollo_visit_identified.json" http://127.0.0.1:8080/apollo/visit)
[ "$code" = 401 ] || fail "a wrong secret got $code, want 401"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Leadscore-Secret: $secret" \
  --data-binary @"$repo/testdata/events/apollo_visit_identified.json" http://127.0.0.1:8080/apollo/visit)
[ "$code" = 200 ] || fail "the golden visit got $code, want 200"

# The next timer run (schedule: 1m) creates the visitor's lead from it.
wait_for 150 "a later timer run picked up the webhook" sh -c "docker compose exec -T leadscore leadscore ranked | grep -q 'lee.park@example.net'"

# The compose health check runs `leadscore healthz`.
wait_for 120 "docker reports the container healthy" sh -c "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $cid)\" = healthy ]"

# The export list was written through the ./out bind mount.
sudo test -s out/nurture.csv || fail "no export list in ./out"

# SIGTERM: serve saves and exits 0 well inside the grace period.
dc stop leadscore
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
[ "$exit_code" = 0 ] || fail "serve exited $exit_code after SIGTERM, want 0"
dc logs --no-color leadscore | grep -q 'leadscore serve: stopped' || fail "no clean shutdown line in the log"

dc logs --no-color leadscore
dc down -v
echo "PASS: docker compose up and a timer run end to end"
