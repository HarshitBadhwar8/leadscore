#!/usr/bin/env bash
# setup/gcp.sh sets leadscore up on Google Cloud (contracts section 9.1): one
# subcommand per runbook step. Run it from the folder holding leadscore.yml,
# on macOS, Linux or Google Cloud Shell, with gcloud installed and logged in
# as a person who may create service accounts, secrets, Cloud Run services and
# jobs, Cloud Scheduler jobs and Artifact Registry repositories.
#
#   setup/gcp.sh [--dry-run] [--config <leadscore.yml>] <step> [arguments]
#
#   accounts [--project P] [--region R]   enable the APIs, create the run and
#                                         receiver accounts, write `hosting`,
#                                         and end with the impersonated login
#   bucket                                create the lease bucket (store.lease_bucket,
#                                         by default <project>-leadscore-lease)
#   secrets                               create the secrets and add the keys
#   deploy <image>                        deploy the receiver service and the run job
#   redeploy [--finish-rotation]          deploy again with hosting.image (after
#                                         adding a key, rotating a secret or changing
#                                         the deadline); --finish-rotation detaches
#                                         the previous receiver secret
#   schedule                              create the scheduler account and job
#
# --dry-run (or LEADSCORE_GCP_DRY_RUN=1) prints every command that would
# change something instead of running it. Commands that only read (describe,
# list) still run, so the plan matches the project as it is.
#
# leadscore.yml is read and written only through `leadscore config get` and
# `leadscore config set-hosting`. The script never prints a key: keys are read
# from the variable of the same name when it is set, else typed at a hidden
# prompt, and handed to gcloud on its standard input.
set -euo pipefail

GCLOUD=${GCLOUD:-gcloud}
LEADSCORE=${LEADSCORE:-leadscore}
DRY_RUN=${LEADSCORE_GCP_DRY_RUN:-}
CONFIG=

# Fixed names (contracts section 3, `hosting`).
SERVICE=leadscore-receiver
JOB=leadscore-run
SCHEDULER_JOB=leadscore-schedule
PROXY_REPO=ghcr-proxy
SCHEDULER_ACCOUNT=leadscore-scheduler
CONFIG_SECRET=leadscore-config
CONFIG_VERSION_SECRET=leadscore-config-version
# The variable the run job reads the bundle's version number into.
CONFIG_VERSION_SECRET_VAR=LEADSCORE_CONFIG_VERSION
RECEIVER_SECRET=receiver-secret
RECEIVER_SECRET_PREVIOUS=receiver-secret-previous
DEFAULT_REGION=asia-south1
DEFAULT_RUN_ACCOUNT=leadscore-run
DEFAULT_RECEIVER_ACCOUNT=leadscore-receiver
# The save budget after the deadline (contracts section 11), in seconds.
SAVE_BUDGET_SECONDS=90
# The key variables and the secrets holding them (internal/hosting KeySecrets).
KEY_VARIABLES="APOLLO_API_KEY HUBSPOT_TOKEN"

die() {
  echo "setup/gcp.sh: $*" >&2
  exit 1
}

say() { echo "==> $*"; }

# run executes a command that changes something; with --dry-run it prints it.
run() {
  if [[ $1 == gc ]]; then
    shift
    set -- "$GCLOUD" "$@"
  fi
  if [[ -n $DRY_RUN ]]; then
    printf '+'
    printf ' %q' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

# run_with_secret pipes a secret value into a command that changes something,
# so the value is never an argument (visible to other users in `ps`) and never
# printed, even with --dry-run.
run_with_secret() {
  local value=$1
  shift
  if [[ $1 == gc ]]; then
    shift
    set -- "$GCLOUD" "$@"
  fi
  if [[ -n $DRY_RUN ]]; then
    printf '+ printf %%s <hidden> |'
    printf ' %q' "$@"
    printf '\n'
    return 0
  fi
  printf '%s' "$value" | "$@"
}

gc() { "$GCLOUD" "$@"; }

# ls_config runs the leadscore CLI with the --config given to this script.
ls_config() {
  if [[ -n $CONFIG ]]; then
    "$LEADSCORE" --config "$CONFIG" "$@"
  else
    "$LEADSCORE" "$@"
  fi
}

# cfg prints a leadscore.yml value, or nothing when it is not set.
cfg() { ls_config config get "$1" 2>/dev/null || true; }

set_hosting() {
  if [[ -n $CONFIG ]]; then
    run "$LEADSCORE" --config "$CONFIG" config set-hosting "$@"
  else
    run "$LEADSCORE" config set-hosting "$@"
  fi
}

# duration_seconds prints a leadscore.yml duration (15m, 1h30m, 1d) in
# seconds. It takes whole numbers of days, hours, minutes and seconds; any
# other form is refused.
duration_seconds() {
  local s=$1 total=0 n u re='^([0-9]+)(d|h|m|s)(.*)$'
  [[ -n $s ]] || return 1
  while [[ -n $s ]]; do
    [[ $s =~ $re ]] || return 1
    n=$((10#${BASH_REMATCH[1]}))
    u=${BASH_REMATCH[2]}
    s=${BASH_REMATCH[3]}
    case $u in
      d) total=$((total + n * 86400)) ;;
      h) total=$((total + n * 3600)) ;;
      m) total=$((total + n * 60)) ;;
      s) total=$((total + n)) ;;
    esac
  done
  echo "$total"
}

# cron_for prints `schedule` as Cloud Scheduler cron (contracts section 3), the
# same conversion as hosting.Cron in Go; a test holds the two equal.
cron_for() {
  local secs m h
  secs=$(duration_seconds "$1") || return 1
  ((secs > 0 && secs % 60 == 0)) || return 1
  m=$((secs / 60))
  if ((m < 60)); then
    ((60 % m == 0)) || return 1
    echo "*/$m * * * *"
    return 0
  fi
  if ((m == 1440)); then
    echo "0 0 * * *"
    return 0
  fi
  ((m < 1440 && m % 60 == 0)) || return 1
  h=$((m / 60))
  ((24 % h == 0)) || return 1
  echo "0 */$h * * *"
}

account_email() {
  case $1 in
    *@*) echo "$1" ;;
    *) echo "$1@$2.iam.gserviceaccount.com" ;;
  esac
}

# The hosting block, read once per step.
load_hosting() {
  PROJECT=$(cfg hosting.project)
  REGION=$(cfg hosting.region)
  [[ -n $PROJECT && -n $REGION ]] || die "hosting.project and hosting.region are not set; run setup/gcp.sh accounts first"
  RUN_SA=$(account_email "$(cfg hosting.run_account)" "$PROJECT")
  RECEIVER_SA=$(account_email "$(cfg hosting.receiver_account)" "$PROJECT")
  [[ $RUN_SA != "@"* && $RECEIVER_SA != "@"* ]] || die "hosting.run_account or hosting.receiver_account is not set; run setup/gcp.sh accounts first"
}

ensure_service_account() { # <email> <display name>
  local email=$1 name=${1%%@*}
  if gc iam service-accounts describe "$email" --project "$PROJECT" >/dev/null 2>&1; then
    say "service account $email exists"
  else
    run gc iam service-accounts create "$name" --project "$PROJECT" --display-name "$2"
  fi
}

ensure_secret() {
  if gc secrets describe "$1" --project "$PROJECT" >/dev/null 2>&1; then
    say "secret $1 exists"
  else
    run gc secrets create "$1" --project "$PROJECT" --replication-policy automatic
  fi
}

# has_version: the secret has an enabled version. A list that fails (no
# access, no network) stops the script rather than reading as "no version".
has_version() {
  local out
  out=$(gc secrets versions list "$1" --project "$PROJECT" --filter "state=ENABLED" --limit 1 --format "value(name)") ||
    die "cannot list the versions of secret $1 (see gcloud's error above)"
  [[ -n $out ]]
}

grant_secret() { # <secret> <account email> <role>
  run gc secrets add-iam-policy-binding "$1" --project "$PROJECT" \
    --member "serviceAccount:$2" --role "$3" --condition None --quiet
}

check_timing() {
  local schedule deadline sched_secs deadline_secs
  schedule=$(cfg schedule)
  deadline=$(cfg deadline)
  CRON=$(cron_for "$schedule") || die "schedule $schedule cannot run on Cloud Scheduler: use whole minutes dividing 60 (5m, 10m, 15m, 30m) or hours dividing 24 (1h, 2h, 6h, 24h)"
  sched_secs=$(duration_seconds "$schedule")
  deadline_secs=$(duration_seconds "$deadline") || die "deadline $deadline: write it in whole days, hours, minutes or seconds, like 12m"
  TASK_TIMEOUT=$((deadline_secs + SAVE_BUDGET_SECONDS))
  ((TASK_TIMEOUT < sched_secs)) || die "deadline $deadline plus the ${SAVE_BUDGET_SECONDS}s save budget is not below schedule $schedule: runs would overlap"
}

step_accounts() {
  # Never gcloud's active project: setting up the wrong project is costly.
  PROJECT=${OPT_PROJECT:-$(cfg hosting.project)}
  [[ -n $PROJECT ]] || die "hosting.project is not set: pass --project <id>"
  REGION=${OPT_REGION:-$(cfg hosting.region)}
  REGION=${REGION:-$DEFAULT_REGION}
  local run_name recv_name person member
  run_name=$(cfg hosting.run_account)
  recv_name=$(cfg hosting.receiver_account)
  run_name=${run_name:-$DEFAULT_RUN_ACCOUNT}
  recv_name=${recv_name:-$DEFAULT_RECEIVER_ACCOUNT}
  RUN_SA=$(account_email "$run_name" "$PROJECT")
  RECEIVER_SA=$(account_email "$recv_name" "$PROJECT")

  say "enabling the APIs in $PROJECT"
  run gc services enable --project "$PROJECT" \
    run.googleapis.com cloudscheduler.googleapis.com secretmanager.googleapis.com \
    storage.googleapis.com sheets.googleapis.com drive.googleapis.com \
    artifactregistry.googleapis.com iam.googleapis.com iamcredentials.googleapis.com

  ensure_service_account "$RUN_SA" "leadscore runs"
  ensure_service_account "$RECEIVER_SA" "leadscore receiver"

  # Cloud Run Viewer and Cloud Scheduler Viewer, for the hosting check.
  local role
  for role in roles/run.viewer roles/cloudscheduler.viewer; do
    run gc projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$RUN_SA" \
      --role "$role" --condition None --quiet
  done

  # The person may act as the run account for local commands.
  person=$(gc config get-value account 2>/dev/null || true)
  [[ -n $person ]] || die "gcloud has no logged-in account; run gcloud auth login"
  case $person in
    *.gserviceaccount.com) member="serviceAccount:$person" ;;
    *) member="user:$person" ;;
  esac
  run gc iam service-accounts add-iam-policy-binding "$RUN_SA" --project "$PROJECT" \
    --member "$member" --role roles/iam.serviceAccountTokenCreator --quiet

  set_hosting "project=$PROJECT" "region=$REGION" "run_account=$run_name" "receiver_account=$recv_name"

  say "signing your local commands in as the run account (a browser opens)"
  run gc auth application-default login --impersonate-service-account "$RUN_SA"
  say "note: Google's application-default login on this machine now acts as $RUN_SA for every program that uses it;"
  say "  undo it with: gcloud auth application-default revoke"
  say "next: setup/gcp.sh bucket"
}

step_bucket() {
  load_hosting
  [[ $(cfg store.type) == sheets ]] || die "the lease bucket is for the Sheets store, and store.type is $(cfg store.type); Google Cloud needs store.type: sheets"
  local bucket
  # Defaults to <project>-leadscore-lease (contracts section 3).
  bucket=$(cfg store.lease_bucket)
  [[ -n $bucket ]] || die "store.lease_bucket is empty; set it to a bucket name of your own"
  if gc storage buckets describe "gs://$bucket" --project "$PROJECT" >/dev/null 2>&1; then
    say "bucket gs://$bucket exists"
  else
    run gc storage buckets create "gs://$bucket" --project "$PROJECT" --location "$REGION" \
      --uniform-bucket-level-access --public-access-prevention ||
      die "could not create gs://$bucket. Bucket names are global: if another project has taken it, set store.lease_bucket in leadscore.yml to another name and run this again"
  fi
  run gc storage buckets add-iam-policy-binding "gs://$bucket" \
    --member "serviceAccount:$RUN_SA" --role roles/storage.objectAdmin
  say "next: leadscore setup sheet (with your own login), then setup/gcp.sh secrets"
}

# read_key prints a key's value: from the variable of the same name, else a
# hidden prompt when run from a terminal. Empty means skip.
read_key() {
  local var=$1 value=
  value=${!var:-}
  if [[ -z $value && -z $DRY_RUN && -t 0 ]]; then
    printf 'Paste %s (input hidden; Enter skips): ' "$var" >/dev/tty
    IFS= read -r -s value </dev/tty || true
    printf '\n' >/dev/tty
  fi
  printf '%s' "$value"
}

secret_for() {
  case $1 in
    APOLLO_API_KEY) echo apollo-api-key ;;
    HUBSPOT_TOKEN) echo hubspot-token ;;
  esac
}

step_secrets() {
  load_hosting
  local s var secret value
  for s in "$CONFIG_SECRET" "$CONFIG_VERSION_SECRET" apollo-api-key hubspot-token "$RECEIVER_SECRET" "$RECEIVER_SECRET_PREVIOUS"; do
    ensure_secret "$s"
  done

  # Contracts section 9, the role table.
  for s in apollo-api-key hubspot-token "$CONFIG_SECRET" "$CONFIG_VERSION_SECRET"; do
    grant_secret "$s" "$RUN_SA" roles/secretmanager.secretAccessor
  done
  for s in "$CONFIG_SECRET" "$CONFIG_VERSION_SECRET"; do
    grant_secret "$s" "$RUN_SA" roles/secretmanager.secretVersionAdder
  done
  # So config push can see the version secret exists before it uploads.
  grant_secret "$CONFIG_VERSION_SECRET" "$RUN_SA" roles/secretmanager.viewer
  for s in "$RECEIVER_SECRET" "$RECEIVER_SECRET_PREVIOUS" "$CONFIG_SECRET"; do
    grant_secret "$s" "$RECEIVER_SA" roles/secretmanager.secretAccessor
  done

  for var in $KEY_VARIABLES; do
    secret=$(secret_for "$var")
    if has_version "$secret"; then
      say "$secret already holds a key; to rotate it, set $var and run this again, then setup/gcp.sh redeploy"
      [[ -n ${!var:-} ]] || continue
    fi
    value=$(read_key "$var")
    if [[ -z $value && -z $DRY_RUN ]]; then
      say "skipped $var (leave it out if no adapter in leadscore.yml uses it)"
      continue
    fi
    run_with_secret "$value" gc secrets versions add "$secret" --project "$PROJECT" --data-file=-
    if gc run jobs describe "$JOB" --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
      say "added $var: the run job is already deployed, so run setup/gcp.sh redeploy to give it the key"
    fi
  done

  if has_version "$RECEIVER_SECRET"; then
    say "$RECEIVER_SECRET already holds a secret"
  else
    # Generated here and never printed: it works like a password (contracts
    # section 5.1, "Keep the secret private").
    value=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
    run_with_secret "$value" gc secrets versions add "$RECEIVER_SECRET" --project "$PROJECT" --data-file=-
    say "generated the receiver secret; paste it only into the Apollo workflows, reading it with:"
    say "  gcloud secrets versions access latest --secret $RECEIVER_SECRET --project $PROJECT"
  fi
  say "next: write the rubric, leadscore config push, then setup/gcp.sh deploy <image>"
}

step_deploy() {
  local image=$1 finish_rotation=${2:-} deploy_image
  load_hosting
  [[ -n $image ]] || die "usage: setup/gcp.sh deploy <image>"
  check_timing

  # The bundle each run reads, and its version number, which runs record:
  # both are read at :latest when an execution starts (S0 confirms), so a
  # config push needs no redeploy.
  if ! has_version "$CONFIG_SECRET" || ! has_version "$CONFIG_VERSION_SECRET"; then
    die "$CONFIG_SECRET or $CONFIG_VERSION_SECRET has no version yet; run leadscore config push first"
  fi

  deploy_image=$image
  case $image in
    ghcr.io/*)
      # Cloud Run cannot pull from ghcr.io; a remote repository proxies it
      # (contracts section 9). S0 confirms the proxy works for ghcr.io.
      if gc artifacts repositories describe "$PROXY_REPO" --location "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
        say "repository $PROXY_REPO exists"
      else
        run gc artifacts repositories create "$PROXY_REPO" --project "$PROJECT" --location "$REGION" \
          --repository-format docker --mode remote-repository \
          --remote-repo-config-desc "ghcr.io" --remote-docker-repo "https://ghcr.io"
      fi
      # The hosting check reads that the repository exists.
      run gc artifacts repositories add-iam-policy-binding "$PROXY_REPO" --location "$REGION" --project "$PROJECT" \
        --member "serviceAccount:$RUN_SA" --role roles/artifactregistry.reader --quiet
      deploy_image="$REGION-docker.pkg.dev/$PROJECT/$PROXY_REPO/${image#ghcr.io/}"
      ;;
  esac

  # The receiver: its own account, at most one instance, no sign-in check
  # (Apollo cannot sign in; S0 confirms --no-invoker-iam-check works under
  # common organization policies), the receiver secrets and the bundle. The
  # previous secret is attached while it has an enabled version, until
  # --finish-rotation detaches it (contracts section 5.1). Detach before
  # disabling: S0 confirms a disabled version attached at :latest stops a new
  # instance from starting.
  local recv_secrets="LEADSCORE_RECEIVER_SECRET=$RECEIVER_SECRET:latest"
  has_version "$RECEIVER_SECRET" || say "warning: $RECEIVER_SECRET has no version, so every webhook gets 401; run setup/gcp.sh secrets"
  if [[ -n $finish_rotation ]]; then
    say "detaching $RECEIVER_SECRET_PREVIOUS; once this deploy is done, disable its versions:"
    say "  gcloud secrets versions disable latest --secret $RECEIVER_SECRET_PREVIOUS --project $PROJECT"
  elif has_version "$RECEIVER_SECRET_PREVIOUS"; then
    recv_secrets="$recv_secrets,LEADSCORE_RECEIVER_SECRET_PREVIOUS=$RECEIVER_SECRET_PREVIOUS:latest"
    say "attaching $RECEIVER_SECRET_PREVIOUS: once every Apollo workflow sends the new secret, run setup/gcp.sh redeploy --finish-rotation"
  fi
  recv_secrets="$recv_secrets,/config/bundle.yaml=$CONFIG_SECRET:latest"
  run gc run deploy "$SERVICE" --project "$PROJECT" --region "$REGION" --image "$deploy_image" \
    --service-account "$RECEIVER_SA" --args serve --min-instances 0 --max-instances 1 \
    --no-invoker-iam-check --set-secrets "$recv_secrets" --quiet

  # The run job: the run account, `leadscore run`, the deadline plus the save
  # budget, no retries (the next scheduled run is the retry), the keys that
  # exist and the bundle.
  local job_secrets="" var secret
  for var in $KEY_VARIABLES; do
    secret=$(secret_for "$var")
    if has_version "$secret"; then
      job_secrets="$job_secrets$var=$secret:latest,"
    fi
  done
  job_secrets="$job_secrets$CONFIG_VERSION_SECRET_VAR=$CONFIG_VERSION_SECRET:latest,/config/bundle.yaml=$CONFIG_SECRET:latest"
  # Memory: S14b's live check measures what a large Sheet needs.
  run gc run jobs deploy "$JOB" --project "$PROJECT" --region "$REGION" --image "$deploy_image" \
    --service-account "$RUN_SA" --args run --tasks 1 --parallelism 1 --max-retries 0 \
    --task-timeout "${TASK_TIMEOUT}s" --memory 1Gi --set-secrets "$job_secrets" --quiet

  set_hosting "image=$image"
  local url
  url=$(gc run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" --format "value(status.url)" 2>/dev/null || true)
  say "receiver: ${url:-<the service URL>}"
  say "next: set receiver.public_url to that address in leadscore.yml and run leadscore config push again;"
  say "  create the Apollo workflows from setup/apollo/ pointing at it; then setup/gcp.sh schedule"
}

step_redeploy() {
  local image
  image=$(cfg hosting.image)
  [[ -n $image ]] || die "hosting.image is not set; run setup/gcp.sh deploy <image> first"
  step_deploy "$image" "${1:-}"
}

step_schedule() {
  load_hosting
  check_timing
  local sched_sa uri
  sched_sa=$(account_email "$SCHEDULER_ACCOUNT" "$PROJECT")
  ensure_service_account "$sched_sa" "leadscore scheduler"
  run gc run jobs add-iam-policy-binding "$JOB" --project "$PROJECT" --region "$REGION" \
    --member "serviceAccount:$sched_sa" --role roles/run.invoker --quiet
  # Cloud Scheduler calls the job's :run endpoint as its own account (S0
  # confirms), in the Cloud Run region (S0 confirms Scheduler is offered there).
  uri="https://run.googleapis.com/v2/projects/$PROJECT/locations/$REGION/jobs/$JOB:run"
  local verb=create
  if gc scheduler jobs describe "$SCHEDULER_JOB" --location "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    verb=update
  fi
  run gc scheduler jobs "$verb" http "$SCHEDULER_JOB" --project "$PROJECT" --location "$REGION" \
    --schedule "$CRON" --time-zone UTC --uri "$uri" --http-method POST \
    --oauth-service-account-email "$sched_sa" --quiet
  say "runs start on \"$CRON\" (UTC); pushes stay off until pushes_enabled: true and leadscore config push"
  say "next: leadscore doctor until green, then open the Ranked tab"
}

# usage prints the comment block at the top of this file.
usage() {
  awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
}

main() {
  OPT_PROJECT=
  OPT_REGION=
  local finish_rotation=
  local step=
  local args=()
  while (($#)); do
    case $1 in
      --dry-run) DRY_RUN=1 ;;
      --config) CONFIG=${2:?--config needs a file}; shift ;;
      --config=*) CONFIG=${1#--config=} ;;
      --project) OPT_PROJECT=${2:?--project needs a value}; shift ;;
      --region) OPT_REGION=${2:?--region needs a value}; shift ;;
      --finish-rotation) finish_rotation=1 ;;
      -h | --help | help) usage; return 0 ;;
      -*) die "unknown flag $1" ;;
      *)
        if [[ -z $step ]]; then step=$1; else args+=("$1"); fi
        ;;
    esac
    shift
  done
  command -v "$GCLOUD" >/dev/null 2>&1 || die "gcloud is not installed (https://cloud.google.com/sdk/docs/install)"
  command -v "$LEADSCORE" >/dev/null 2>&1 || die "the leadscore CLI is not installed (see the README)"
  [[ -z $finish_rotation || $step == redeploy ]] || die "--finish-rotation goes with redeploy"
  # Once, first: leadscore.yml must load, or every value read below is empty.
  local loaded
  loaded=$(ls_config config get version 2>&1) || die "leadscore.yml does not load: $loaded"
  [[ -z $DRY_RUN ]] || say "dry run: printing every change instead of making it"
  case $step in
    accounts) step_accounts ;;
    bucket) step_bucket ;;
    secrets) step_secrets ;;
    deploy) step_deploy "${args[0]:-}" ;;
    redeploy) step_redeploy "$finish_rotation" ;;
    schedule) step_schedule ;;
    "") usage; return 2 ;;
    *) die "unknown step $step (accounts, bucket, secrets, deploy, redeploy, schedule)" ;;
  esac
}

# Sourced (tests), it only defines the functions.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
