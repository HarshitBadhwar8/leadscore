# leadscore

An open-source outbound engine: bring leads in, merge them into people, score them
with your own YAML rubric, route each to one lane, and push them to your tools.

Work in progress. The design lives in `docs/design/` (the RFC, the contracts, and
the task breakdown); the contracts doc is the source of truth for every shape.

## Layout

| Path | What lives there |
|---|---|
| module root (package `leadscore`) | the public API: plug-in interfaces, types, errors, the adapter registry, `Run`, `Main` |
| `cmd/leadscore/` | the CLI binary |
| `internal/api` | the public types (re-exported by the root), the built-in header aliases |
| `internal/config` | loading `leadscore.yml`, `config get`, `config set-hosting` |
| `internal/check` | the `doctor` check framework and the `secrets`, `receiver-secret`, `store`, `rubric`, `overrides` and `duplicates` checks |
| `internal/model` | the in-memory model of the store's tables |
| `internal/store/codec` | maps the model to table writes; loads a store and checks its schema version |
| `internal/store/sqlite` | the built-in SQLite store (WAL, lease row, event log) |
| `internal/store/sheets` | the built-in Google Sheets store (one batchUpdate per commit, monthly `Events` tabs, Cloud Storage lease file) and `setup sheet` |
| `internal/fakes/sheets`, `internal/fakes/gcs` | in-memory fakes of Google Sheets, Drive and Cloud Storage for tests |
| `internal/rules` | the rubric compiler and evaluator: YAML rules compiled to CEL |
| `internal/merge` | turns input rows into one lead per person: header aliases, identities, `same_as` merges, the Overrides tab |
| `internal/receiver` | `leadscore serve`: the Apollo webhook receiver, its write queue, `/healthz` and the Docker run timer |
| `internal/receiver/auth` | the constant-time secret check |
| `internal/engine` | the run: lease, sources and chunked merge, scoring, the two saves, `Ranked`, `Health`, lanes and the ledger, export lists and their CSVs; later steps plug in as hooks |
| `internal/logredact` | log redaction: logs carry ids, never emails |
| `internal/hosting` | Google Cloud: the fixed resource names, Secret Manager reads and writes (`config push`, hosted keys), the schedule's cron form, and the reads behind the `hosting` check |
| `internal/fakes/gcp` | an in-memory fake of Secret Manager, Cloud Run, Cloud Scheduler and Artifact Registry for tests |
| `adapters/csv` | the CSV file source (`type: csv`): lead rows, or event rows with `events: true` |
| `adapters/sheetsource` | the Google Sheet tab source (`type: sheetsource`): tabs of the team's spreadsheet |
| `adapters/hubspot` | the HubSpot sink (`hubspot:contacts`, `hubspot:deals`), the opt-out and deal lookup, `setup hubspot` and the `hubspot` check |
| `internal/fakes/hubspot` | a fake HubSpot portal for tests, held to the provisional fixtures in `testdata/vendors/hubspot` |
| `storetest/`, `sinktest/` | conformance suites for plug-in stores and sinks |
| `examples/` | a made-up example rubric (`rubric.yml`), a sample lead sheet (`leads.csv`), and example `leadscore.yml` files for Docker on a laptop and on a server, and for Google Cloud |
| `compose.yaml`, `Dockerfile` | the Docker setup: the image and the compose file that runs it |
| `setup/apollo/` | the Apollo workflow templates the receiver accepts |
| `setup/gcp.sh` | the Google Cloud setup script, one subcommand per runbook step |

## Build and test

```sh
go build ./...
go vet ./...
go test -race ./...
```

`LEADSCORE_LIVE_SHEETS=1` (or the path of a service-account key file) also runs the
live Sheets check: it saves 20,000 leads and a year of events to a scratch
spreadsheet and loads them within a minute. Without it the check is skipped.

`LEADSCORE_LIVE_CLOUDRUN=1`, with `LEADSCORE_LIVE_PROJECT` (a billed, throwaway
project, never `leadscore-dev`) and `LEADSCORE_LIVE_IMAGE`, runs the live
Google Cloud check: it sets the project up with `setup/gcp.sh`, starts a run
longer than 3 minutes from Cloud Scheduler, posts webhook bursts during the run
and during a redeploy, and checks no event was lost. Its comment lists what to
do first. Without it the check is skipped. `setup/gcp.sh` itself is tested
offline against a fake `gcloud` (`testdata/gcloud/gcloud`).

## Commands

`leadscore help` lists every command. So far `run`, `serve`, `healthz`, `status`, `ranked`,
`explain`, `config get`, `config set-hosting`, `config push`, `rules check`, `setup sheet`,
`setup hubspot` and the Overrides writers work; every other command prints `not built yet (slice S<n>)` and
exits 2.

- `leadscore run`: one run. It reads `leadscore.yml` and the rubric fresh,
  takes the run lease (another run holding it means this one is skipped),
  merges new input rows (`ingest_chunk_rows` per run; a large first import
  finishes over several runs, and nothing is pushed until it has), scores
  every lead, and saves `Ranked` and `Health`. It exits 1 when the run failed
  or finished unhealthy. A lane whose sink is not in the build is reported
  in `Health`.
- `leadscore run --dry-run`: scores every row in memory and prints one line
  per lead whose verdict, status or planned lane would change, then totals. It
  takes no lease and writes nothing.
- `leadscore serve [--every [interval]]`: the always-on receiver for Apollo
  workflow webhooks (see "The receiver" below) and `/healthz`. With `--every`
  it also runs the loop: once at start, then each `interval` (or `schedule`
  from `leadscore.yml`, read at start) after the previous run ended, so runs
  never overlap. On SIGTERM it lets a running run save, stores every webhook
  it accepted, and exits.
- `leadscore healthz`: calls the local `/healthz` and exits 0 when it answers
  200 (the compose health check).
- `leadscore status`: the last run's result and every open problem.
- `leadscore ranked [--csv]`: every lead's verdict, highest score first.
- `leadscore explain <person>`: one lead's verdict and the reasons behind it.

The Overrides writers edit the `Overrides` table the same way on every store.
A person is an email, a LinkedIn URL or a lead id; a row for someone not yet
imported waits until they appear.

- `leadscore set-status <person> <status|none|resubscribe>`: replace the
  person's manual status (`replied_*`, `unsubscribed`, `blocked`), remove it, or
  undo a manual `unsubscribed`.
- `leadscore merge <person> <person>`: the two are one person; the next run
  merges them for good.
- `leadscore mark-distinct <person> <person>`: two people who share a company
  and a name are different people.
- `leadscore retry [--lane <id>] [<person>]`: retry failed pushes, for one
  person or everyone, in one lane or all.

`leadscore setup sheet` creates the Sheets store's spreadsheet with your own Google
login (`gcloud auth login --enable-gdrive-access` first), shares it with the run and
receiver accounts, and writes `store.spreadsheet`; `--view` makes a SQLite store's
read-only view; `--repair` puts an existing spreadsheet's settings back.

## HubSpot

Add a `sinks.hubspot` block and set `HUBSPOT_TOKEN` to a private app's token:

```yaml
sinks:
  hubspot: { pipeline: Sales Pipeline, stage: Appointment scheduled }
```

`leadscore setup hubspot` creates the custom properties (`leadscore_lead_id` and
friends, in a `leadscore` group) and checks that the pipeline and stage exist;
run it once, and again after changing `property_prefix`. It needs the schema write
scopes; runs need contacts and deals read and write, companies read, and schema read.

A lane pushing to `hubspot:contacts` finds or creates the person's contact; one
pushing to `hubspot:deals` also opens one deal per company (named by its domain) or
adds the contact to the company's open deal. Before every push, leadscore reads
HubSpot for the leads it may push: a contact that opted out of email makes the
lead `unsubscribed`, and a company with an open or won deal keeps its people out of
cold lanes until the deal is closed lost.

## Enrichment

With an `enrich` block, each run looks up company facts (name, headcount,
funding stage, country, latest funding date) in Apollo for the companies of
your leads, using `APOLLO_API_KEY`:

```yaml
enrich: { type: apollo, max_age: 30d, max_lookups_per_run: 100, max_lookups_per_day: 400 }
```

A company is looked up when it has never been, or when its last lookup (or
Apollo's "not found") is older than `max_age`. Every call counts toward both
budgets, so a large first import spreads over several days instead of
spending a month of credits at once. Your `Companies` tab always wins over
Apollo; Apollo's value wins over a value from a lead sheet. When a value
changes, the old one is kept in `Company facts` as `previous`. A dry run makes
no lookups. A lookup that gets no answer waits a day, and three in a row stop
enrichment for that run (the warning `enrich_failed`). The `apollo-key` check
signs in with Apollo's free auth-health call on every run, so a bad key shows
in `Health` without spending a credit.

**If you send only through Apollo:** leadscore does not yet know whether
Apollo's contacts carry an opt-out flag it can read. Until that is confirmed, a
person who clicked an unsubscribe link without replying may not be seen before
a push, unless HubSpot is also a sink or the receiver gets Apollo's
`unsubscribed` webhook. The `apollo-key` check warns
(`apollo-key:no_optout_flag`) until an unsubscribe webhook has been received.

## Export lists

An `export` lane in the rubric keeps a list instead of pushing to a tool: one
table per lane (`Export <lane id>`; a tab on a Sheets store) and, on SQLite, one
CSV per lane in `export.dir` (`./out` on Docker), rewritten after every run.
A lead is listed once. Every run refreshes each row's `status` and
`do_not_contact`, also for lanes since removed from the rubric. **Filter on
`do_not_contact` before every send**: it is `yes` for anyone opted out, blocked,
at a company with an open deal, already contacted, or headed for a cold lane.
Opt-outs reach the list from the receiver, polling and Overrides; the vendor
opt-out lookups run only for leads about to be pushed.

A cold lane claims its leads even before its sink is set up: anyone it matches
is `do_not_contact`. So a CSV-only team removes the cold lanes from its rubric
and lists leads only through export lanes.

Without `--config`, commands read `/config/bundle.yaml`, else
`/config/leadscore.yml`, else `./leadscore.yml`.

## The receiver

`leadscore serve` accepts Apollo workflow requests at `POST /apollo/reply`
(sent, replies, opt-outs) and `POST /apollo/visit` (website visits), with
bodies from the templates in `setup/apollo/`. Each request carries the
receiver secret in the `X-Leadscore-Secret` header (or, when Apollo cannot set
headers, in a top-level `leadscore_secret` body field, which is removed before
storing; prefer the header, since a flood of slow senders can delay
body-secret requests but never header ones). The receiver answers 200 only once the event is stored; a wrong or
missing secret gets 401, and a request it could not store within 10 seconds
gets 503 so Apollo can send it again. A repeated event is counted once.

`/healthz` with the timer is 200 while the last run succeeded or none is due
yet, and 503 when the last run failed or none succeeded in three intervals;
without the timer (Google Cloud) it is 200 unless storing events fails.

### Keep the receiver secret private

The receiver secret (`LEADSCORE_RECEIVER_SECRET`) works like a password:
anyone who has it can send fake events, including a fake positive reply that
a deal lane would act on. Never paste it into chat, tickets or shared docs;
type it only into `.env` (or Secret Manager) and the Apollo workflows. Rotate
it if it might have leaked. Without it every webhook is refused, while
`/healthz` and the timer keep working.

**Rotating it:** move the current value to
`LEADSCORE_RECEIVER_SECRET_PREVIOUS`, set a new `LEADSCORE_RECEIVER_SECRET`,
update each Apollo workflow, then remove the previous one. Both are accepted
in between, so no webhook is lost. On Docker, edit `.env` and run
`docker compose up -d` after each change. On Google Cloud, see "Rotating a
secret" under Google Cloud below.

## Docker

For technical users, on your own machine or a server. Put `compose.yaml`, an
example `leadscore.yml` (`examples/leadscore.laptop.yml` or
`examples/leadscore.server.yml`, renamed), your rubric and your CSV files in
one folder, add a `.env` with your keys and the receiver secret (see "Keep
the receiver secret private" above), and run `docker compose up -d`. Until the
first release, `.env` also sets `LEADSCORE_IMAGE` to the private registry's
image; remove that line after release. Upgrading is changing the image tag in
`compose.yaml` (or `LEADSCORE_IMAGE`, which overrides it) and running
`docker compose up -d`, then `doctor`. The compose file assumes the receiver's
port 8080 inside the container; leave `receiver.port` unset. The folder is mounted read-only at `/config`, the
SQLite store lives on a named volume, and the export lists land in `./out`
(on Linux, first `mkdir -p out && sudo chown 10001:10001 out`, since the
container runs as its own user, uid 10001).

On Linux the container's user must be able to read the folder and the files
it reads: `chmod o+rx . && chmod o+r leadscore.yml rubric.yml *.csv` (`.env`
is read by Docker, not the container, so leave it private). The export lists
are written owner-only (mode 0600, they hold personal data) and owned by uid
10001, so read them with `sudo cat out/<lane>.csv`, or copy one out with
`docker compose cp leadscore:/out/<lane>.csv .`. Every command runs inside the container:
`docker compose exec leadscore leadscore status`. `docker ps` shows the
container unhealthy when `/healthz` does.

On a laptop there is no public address, so the laptop example polls Apollo for
replies (`replies: polling`). Polling needs a `sinks.apollo` block, which the
examples do not have yet: until it is added, every run reports `poll_failed`
and the container shows unhealthy. A Cloudflare Tunnel can give it a public URL for
live webhooks, but only while the laptop is awake. On a server, the compose
file's optional Caddy service (`docker compose --profile caddy up -d`) gets an
HTTPS certificate for your domain.

## Google Cloud

The path for non-technical teams: the receiver runs as a Cloud Run service,
each run as a Cloud Run job that Cloud Scheduler starts, and the store is a
Google Sheet. Nothing runs on your machine after setup. Billing must be on.

You need `gcloud`, logged in as someone who may create service accounts,
secrets, Cloud Run services and jobs, Cloud Scheduler jobs and Artifact
Registry repositories, and the `leadscore` CLI. Copy
`examples/leadscore.gcp.yml` (as `leadscore.yml`) and your rubric into one
folder and run every step from it. `setup/gcp.sh` changes `leadscore.yml` only
through `leadscore config set-hosting`. Add `--dry-run` to any step to print
the changes it would make without making them.

1. `setup/gcp.sh accounts --project <id> [--region asia-south1]`: enables the
   APIs, creates the run account (`leadscore-run`) and the receiver account
   (`leadscore-receiver`), writes the `hosting` block, and ends by signing your
   local commands in as the run account (a browser opens).
2. Set `store.lease_bucket` (bucket names are global; use your project id in
   it), then `setup/gcp.sh bucket`.
3. `gcloud auth login --enable-gdrive-access`, then `leadscore setup sheet`:
   creates the spreadsheet with your own login, so you own it, and shares it
   with both accounts.
4. `setup/gcp.sh secrets`: creates the secrets and asks for each API key (a
   person pastes it; input is hidden; Enter skips a key you do not use). It
   generates the receiver secret and never prints it; read it only when you
   paste it into the Apollo workflows:
   `gcloud secrets versions access latest --secret receiver-secret`.
5. Write the rubric, `leadscore setup hubspot` if you use HubSpot, then
   `leadscore config push`: uploads `leadscore.yml` and the rubric together as
   one version of the `leadscore-config` secret. It refuses a rubric that does
   not compile, a SQLite store or CSV path (Cloud Run keeps no files), and a
   `schedule` or `deadline` Cloud Scheduler cannot run.
6. `setup/gcp.sh deploy <image>`: the receiver service (at most one instance,
   no sign-in check so Apollo can reach it) and the run job (task timeout the
   deadline plus 90 seconds, no retries). It prints the receiver's address:
   set `receiver.public_url` to it and point the Apollo workflows at it, from
   the templates in `setup/apollo/`. Before release, pass the private
   registry's image; a release image (`ghcr.io/...`) is pulled through an
   Artifact Registry repository, `ghcr-proxy`, that the step creates.
7. `setup/gcp.sh schedule`: creates the scheduler account and the scheduler
   job from `schedule` (UTC). Pushes are still off, so runs only score.
8. `leadscore doctor` until green, then open the `Ranked` tab. Review
   `leadscore run --dry-run`; when it looks right, set `pushes_enabled: true`
   and `leadscore config push`.

**Changing settings or rules** is editing the files and `leadscore config push`;
the next run reads them, with no redeploy. A new `schedule` also needs
`setup/gcp.sh schedule`, and a new `deadline` `setup/gcp.sh redeploy`.
Each run records the bundle version it was deployed with in `State`
(`config_version`); until a redeploy, the `hosting` check reports a newer
pushed version.

**Keys on your machine.** Local commands on a Google Cloud install read an API
key you have not set as a variable from Secret Manager, as the run account,
and only the keys the command's adapters need. Inside Cloud Run the keys come
from the service's and job's own secret references.

**Rotating a secret.** Add a version to `receiver-secret-previous` holding the
current secret, add the new one to `receiver-secret`, and run
`setup/gcp.sh redeploy`: the receiver accepts both. Update each Apollo
workflow, disable the versions of `receiver-secret-previous`, and redeploy
again; the previous secret is attached only while it has an enabled version.
An API key is rotated by setting its variable and running `setup/gcp.sh
secrets`, then `redeploy`.

**Upgrading** is `setup/gcp.sh deploy <new image>`, then `doctor`; rolling
back is deploying the previous image.

### Run time and monthly cost

Not measured yet: this needs a billed project (waiting on Harshit). The live
check (`LEADSCORE_LIVE_CLOUDRUN`) prints the run length; the cost follows from
it. The method, to fill in the table:

1. Run the live check, and note `MEASURE run length` for the synthetic Sheet,
   then the length of an ordinary run on a team-sized Sheet (2,000 leads),
   from `gcloud run jobs executions list --job leadscore-run`.
2. Runs per month at the default 15 minutes: 4 × 24 × 30 = 2,880. The job
   has 1 vCPU and 1 GiB, so a month uses 2,880 × run seconds vCPU-seconds and
   as many GiB-seconds.
3. Compare with Cloud Run's free tier per billing account (240,000
   vCPU-seconds and 450,000 GiB-seconds a month at the time of writing) and
   price the rest from Google's price list for the region. Add Cloud Scheduler
   (three jobs free per billing account), Secret Manager (a few secret
   versions and one access per run per secret) and the receiver (scales to
   zero; billed only while it answers a webhook).

| Measure | Value |
|---|---|
| Run length, 2,000 leads | _to measure_ |
| Run length, live check's synthetic Sheet | _to measure_ |
| vCPU-seconds per month at 15 minutes | _to measure_ |
| Monthly cost beyond the free tier | _to measure_ |

## The rubric

Your ideal customer is one YAML file, the rubric: which columns matter, how to
label each company and person (tier, priority, ...), how to score them, and
where each lead goes. Start from `examples/rubric.yml`; the full format is
section 2 of `docs/design/oss-outbound-engine-contracts.md`. Check a rubric
with `leadscore rules check rubric.yml`, which lists every problem with its line.
