# leadscore

[![CI](https://github.com/HarshitBadhwar8/leadscore/actions/workflows/ci.yml/badge.svg)](https://github.com/HarshitBadhwar8/leadscore/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)

A small sales team gets leads from many places: conference lists, a lead
sheet, website visits. Someone has to merge them, decide who is worth a call,
and make sure nobody who opted out or already replied gets cold email again.
leadscore does that on a schedule. It merges your leads into people, scores
them with a rubric you write in YAML, and sends each lead to one place: an
Apollo sequence, HubSpot, or a plain list you send from yourself.

Status: pre-release. Before v1, the `leadscore.yml` keys, the rubric format and the commands may
change, and [CHANGELOG.md](CHANGELOG.md) says how.

## Contents

- [What it does](#what-it-does) lists the commands and the plug-in API, and what leadscore does not do.
- [Quick start](#quick-start) scores the sample leads on your machine in a few minutes.
- [Words used here](#words-used-here) explains lanes, sinks, `do_not_contact` and the receiver.
- [Choosing a path](#choosing-a-path) maps your team to one of four ways to run it.
- [Set it up](#set-it-up) is the step-by-step runbook for each path.
- [Usage](#usage) covers the export lists and how to check on a running install.
- [Reference](#reference) lists every command and the rules for enrichment, HubSpot and the receiver.
- [Before you rely on it](#before-you-rely-on-it) lists what is not yet checked against the real vendors, and other limits.
- [Project](#project) covers how it ships, versioning, tests, contributing and the license.

## What it does

leadscore is one binary. Each run reads your leads, merges rows that are the
same person, scores every lead, and sends each to at most one lane. It also
reads replies and opt-outs back, so it never cold-contacts an opted-out person
or anyone twice.

- `leadscore run` does one run. Leads come from CSV files, Google Sheet tabs
  and Apollo webhooks. Results go to Apollo sequences, HubSpot contacts and
  deals, or export lists (a CSV file or a Sheet tab).
- `leadscore run --dry-run` prints what a run would change, and writes and
  spends nothing.
- `leadscore serve` is the receiver for Apollo's webhooks: replies, opt-outs
  and website visits. With `--every` it also runs on a timer.
- `leadscore doctor` and `leadscore status` check the install and print each
  problem with its fix.
- `leadscore ranked` and `leadscore explain` show every lead's score and the
  reasons behind it.
- The module root is a Go package, `leadscore`. A custom build can add its own
  source, store or sink as a plug-in ([Plug-ins](docs/reference.md#plug-ins)).

What it does not do:

- It sends no email itself. Apollo, HubSpot or your own tool sends.
- It finds no new leads. You bring them, and Apollo can fill in company facts.
- It has no alerting in v1. Someone has to look ([Health, status and doctor](#health-status-and-doctor)).
- It has no web interface. On Google Cloud, a Google Sheet is the interface.

## Quick start

You need Go 1.25 or later and a terminal on macOS, Linux or Windows (WSL).
No account or key is needed: this scores the made-up leads in
`examples/leads.csv` with the example rubric and writes the lists.

```sh
git clone https://github.com/HarshitBadhwar8/leadscore.git
cd leadscore
go build -o leadscore ./cmd/leadscore
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
../leadscore run
../leadscore ranked --csv | cut -d, -f4,10,12
ls out
```

```text
run <run id>: healthy; 8 lead(s) scored, 8 input row(s) merged, 0 left for later runs, 0 pushed
full_name,score,lane
Anna Weber,65,call-list
Jonas Brandt,60,call-list
Lea de Vries,50,call-list
Pia Schulz,50,call-list
Ines Ruiz,40,call-list
Marie Laurent,15,nurture
Tom Hale,0,
Sam Ortiz,0,
call-list.csv
nurture.csv
```

Each run has its own id. The example rubric has two export lanes, so there
are two lists in `out/`. Tom Hale and Sam Ortiz work at companies the rubric
rules out, so they are on no list.

To see why a lead scored as it did, run
`../leadscore explain anna.weber@kranlogistik.example`. Edit `rubric.yml`
and run again to see the ranking change. When you are ready for your own
leads, follow [Path 1: CSV only](#path-1-csv-only).

## Words used here

- **Lane:** where a lead goes, set in your rubric. Each lead goes to at most
  one lane per run.
- **Cold lane:** first outreach to someone who has not talked to you (an
  Apollo sequence, a new HubSpot contact). A person gets a cold push once,
  ever.
- **Sink:** a tool a lane pushes to: Apollo or HubSpot, set up in the
  `sinks` block of `leadscore.yml`.
- **Export lane:** a lane that only keeps a list (a CSV file or a Sheet tab)
  for you to send from your own tool. It pushes nothing.
- **`do_not_contact`:** a column on every list. `yes` means do not email this
  person. They opted out, were already contacted, are at a company with an
  open deal, or were claimed by a cold lane.
- **Receiver:** the always-on web address Apollo sends replies, opt-outs and
  website visits to (`leadscore serve`).

## Choosing a path

| Path | For | Runs on |
|---|---|---|
| **Google Cloud** | non-technical teams: nothing runs on your machine after setup | Cloud Run and Cloud Scheduler in your own Google Cloud project, with a Google Sheet as the store |
| **Docker on a server** | technical users with a VM and a domain | your server, with Caddy for HTTPS and SQLite as the store (optionally with a Sheet view) |
| **Docker on a laptop** | technical users trying it out, or a small team | your machine while it is awake, with SQLite as the store |
| **CSV only** | a first try, or a team that only wants scored lists | Docker or the plain binary, with SQLite as the store |

Not sure? Start with **CSV only**. It takes ten minutes, needs no accounts,
and every other path builds on it.

Until the first release, every path needs someone comfortable with a
terminal. The CLI is built from source, and Google Cloud setup runs `gcloud`
and `setup/gcp.sh` commands. After release, a non-technical person can follow
Path 2 with a coding agent.

On every path, runs start with **pushes off**. leadscore scores and lists,
you review a dry run, and only then turn pushing on.

**There is no alerting in v1, on any path.** Nobody is emailed or messaged
when a run fails. Someone has to look (`leadscore status`, the `Health` tab,
`docker ps`), or point an uptime monitor at the receiver's `/healthz`.

## Set it up

Follow one path from top to bottom. A coding agent can follow it too, since
each step says who does it.

- **[agent]**: the agent (or you) runs it.
- **[person]**: a person must do it: paste a key, approve billing, create an
  account, or say yes to a dry run. An agent stops and asks.

### Before you start

**[person]** Have ready, for the path you picked:

- **Every path:** your leads as a CSV file (or a Google Sheet tab) with a
  header row. Columns are matched by name. `Email`, `Full Name`, `Job Title`,
  `Company`, `Website`, `LinkedIn` and their usual spellings work as they
  are.
- **Apollo or HubSpot:** a paid Apollo plan (workflows and the API need it)
  and an Apollo master API key, and a HubSpot private app token. CSV only
  needs neither.
- **Docker paths:** Docker (Docker Desktop or Colima on macOS, Docker Engine
  on Linux) or podman with compose.
- **Docker on a server:** a VM with a public IP, ports 80 and 443 open, and a
  domain (or subdomain) whose DNS points at it.
- **Google Cloud:** a Google account that may create a project with billing,
  and `gcloud` installed and logged in (`gcloud auth login`).

### Install the CLI

The Docker paths need no CLI on your machine, because every command runs
inside the container. You need it for **CSV only as a plain binary**, for
**Google Cloud**, and to make a Google Sheet (`setup sheet`) on any path.

- **[agent]** After the first release: download the binary for your OS from
  the repository's releases page and put it on your `PATH` as `leadscore`.
- **[agent]** Before the first release (now): build it from the private
  repository, with Go 1.25 or newer:

  ```sh
  git clone https://github.com/HarshitBadhwar8/leadscore.git
  cd leadscore && go build -o ~/.local/bin/leadscore ./cmd/leadscore
  leadscore help    # check: ~/.local/bin (or the folder you chose) must be on your PATH
  ```

  The pre-release container image also works for most commands. It does not
  work for `setup sheet` (it signs in with your machine's `gcloud`) or
  `setup/gcp.sh` (it calls `leadscore` on your `PATH`):

  ```sh
  docker run --rm -v ~/.config/gcloud:/home/leadscore/.config/gcloud:ro -v "$PWD":/config \
    asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:<tag> doctor
  ```

Every command takes `--config <file>` and `--rubric <file>`. Without
`--config`, commands read `/config/bundle.yaml`, else `/config/leadscore.yml`,
else `./leadscore.yml`. So run them from the folder holding `leadscore.yml`.

### Path 1: CSV only

CSV files in, scored lists out. No vendor accounts.

1. **[agent]** Make a folder for the install and copy in, from this
   repository's `examples/`: `leadscore.csv-only.yml` as `leadscore.yml`,
   `rubric.csv-only.yml` as `rubric.yml`, and your CSV as `leads.csv` (or
   `examples/leads.csv` to try it with made-up leads).
2. **[person]** Write the rubric for your ideal customer. Start from the
   example, and see [The rubric](#the-rubric). Check it with
   `leadscore rules check rubric.yml`. The example rubric has export lanes
   only, on purpose. A cold lane claims the leads it matches even before
   Apollo or HubSpot is set up, and they show `do_not_contact: yes` on every
   list.
3. Run it, as a plain binary or on Docker:
   - **Plain binary. [agent]** In `leadscore.yml`, change `store.path` to
     `leadscore.db` and `export.dir` to `out` (paths are relative to the
     folder). Set a receiver secret, because doctor checks it is set even
     though a CSV-only install receives no webhooks. Keep it for new
     terminals by adding this line to your shell profile (`~/.zshrc` or
     `~/.bashrc`):
     `export LEADSCORE_RECEIVER_SECRET=<the output of openssl rand -hex 32>`.
     Then run `leadscore run --dry-run` to see what a run would do, writing
     nothing. It names leads by id, and after the first real run
     `leadscore explain <lead id>` shows who each is. Then run
     `leadscore run`.
   - **Docker. [agent]** Follow Path 3 below with these files, and skip its
     Apollo and HubSpot steps.
4. **[agent]** Run `leadscore doctor` until it prints `0 failed` (on Docker,
   `docker compose exec leadscore leadscore doctor`). With CSV only, expect
   two warnings: `pushes-enabled:off` and `receivers:no_public_url`.
5. **[agent]** See the results: `leadscore ranked` (every lead, best first),
   `leadscore explain <email>` (why a lead scored as it did), and one CSV per
   export lane in `out/` (`out/call-list.csv`, `out/nurture.csv` with the
   example rubric). **Filter on `do_not_contact` before every send.**

Run `leadscore run` again whenever your CSV changes, or let Docker run it
every 15 minutes. A lead is added to a list once, and each run refreshes its
`status` and `do_not_contact`.

### Path 2: Google Cloud

The receiver runs as a Cloud Run service, and each run as a Cloud Run job
that Cloud Scheduler starts. The store is a Google Sheet you own. Run every
step from one folder on your machine (macOS, Linux or Google Cloud Shell),
with the CLI installed.

`setup/gcp.sh` is in this repository. It changes `leadscore.yml` only through
`leadscore config set-hosting`. Add `--dry-run` to any `setup/gcp.sh` step to
print what it would change without changing it.

1. **[person]** Prerequisites: a paid Apollo plan, and `gcloud` installed and
   logged in as someone who may create projects, service accounts, Cloud Run
   services and jobs, Cloud Scheduler jobs and Artifact Registry
   repositories, and grant roles on them.
2. **[agent]** Install the CLI (above). Copy `examples/leadscore.gcp.yml` as
   `leadscore.yml` and `examples/rubric.yml` as `rubric.yml` into an empty
   folder, and `setup/gcp.sh` beside them (or call it by its path).
3. **[person]** Create or pick a Google Cloud project, and turn billing on
   for it. Google asks a person to approve billing.
4. **[agent]** `setup/gcp.sh accounts --project <id> [--region asia-south1]`
   enables the APIs (Cloud Run, Cloud Scheduler, Secret Manager, Cloud
   Storage, Sheets, Drive, Artifact Registry, IAM Service Account
   Credentials). It creates the run account (`leadscore-run`) and the
   receiver account (`leadscore-receiver`), writes the `hosting` block, and
   ends by signing your local commands in as the run account. **[person]** A
   browser opens: sign in. From then on, Google's application-default login
   on your machine acts as the run account for every program that uses it.
   `gcloud auth application-default revoke` undoes it.
5. **[agent]** `setup/gcp.sh bucket` creates the lease bucket,
   `<project>-leadscore-lease`. Bucket names are global. Only if that name is
   taken, set `store.lease_bucket` to another and run it again.
6. **[person]** `gcloud auth login --enable-gdrive-access` (a browser opens),
   then **[agent]** `leadscore setup sheet`. It creates the spreadsheet with
   your own login, so you own it, shares it with both accounts, and writes
   `store.spreadsheet`. If a Google Workspace sharing policy blocks the
   share, the command says so. **[person]** Then ask your Workspace admin to
   allow sharing this file with the service accounts (an exception for
   `gserviceaccount.com`), and run `leadscore setup sheet --repair`.
7. **[agent]** `setup/gcp.sh secrets` creates the secrets. **[person]** It
   asks for each API key at a hidden prompt. Paste it, or press Enter to skip
   a key you do not use. It generates the receiver secret and never prints it
   (see [Keep the receiver secret private](#keep-the-receiver-secret-private)).
8. **[person]** Write the rubric (see [The rubric](#the-rubric)), and put your
   leads in the spreadsheet's `Leads` tab. **[agent]** If you use HubSpot, add
   the `sinks.hubspot` block and run `leadscore setup hubspot`. Then run
   `leadscore config push`, which uploads `leadscore.yml` and the rubric
   together as one version of the `leadscore-config` secret. It refuses:
   - a rubric that does not compile
   - a SQLite store or CSV path, because Cloud Run keeps no files
   - a `schedule` or `deadline` Cloud Scheduler cannot run (write them like
     `15m`)
   - anything that looks like a key, including the API keys already in
     Secret Manager

   It cannot see the receiver secret, so never paste that into
   `leadscore.yml` or the rubric.
9. **[agent]** `setup/gcp.sh deploy <image>` deploys the receiver service and
   the run job. The service has at most one instance and no sign-in check, so
   Apollo can reach it. The job's task timeout is the deadline plus 90
   seconds, with no retries. Before the first release, `<image>` is the
   private registry's
   `asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:<tag>`, and
   **[person]** a `leadscore-dev` owner grants your project's Cloud Run
   service agent Artifact Registry Reader on that repository. A release image
   (`ghcr.io/tetriz-ai/leadscore:<version>`) is pulled through an Artifact
   Registry repository, `ghcr-proxy`, that this step creates. The step prints
   the receiver's address. **[agent]** Set `receiver.public_url` to it and
   run `leadscore config push` again.
10. **[person]** Create the Apollo workflows from the templates in
    `setup/apollo/` (its README says how), pointing at the receiver's
    address. Read the secret only when you paste it into Apollo:
    `gcloud secrets versions access latest --secret receiver-secret`.
11. **[agent]** `setup/gcp.sh schedule` creates the scheduler account and the
    scheduler job from `schedule` (UTC). Pushes are still off, so these runs
    only score.
12. **[agent]** Run `leadscore doctor` until it prints `0 failed`. Warnings
    are fine, but read each one. Then open the spreadsheet's `Ranked` tab.
13. **[agent]** Run `leadscore run --dry-run` and show the result to a person.
    It names leads by id, and `leadscore explain <lead id>` shows who each
    is. **[person]** Approve it, or change the rubric and repeat.
14. **[agent]** Only after that approval: set `pushes_enabled: true` and run
    `leadscore config push`. The next run pushes.

**Changing settings or rules later** means editing the files and running
`leadscore config push`. The next run reads them, with no redeploy, and
`SKILL.md` has the full loop. A new `schedule` also needs
`setup/gcp.sh schedule`, and a new `deadline` needs `setup/gcp.sh redeploy`.
Each run records the bundle version it read in `State` (`config_version`),
and `doctor`'s `rubric-version` line warns when the last run used another
rubric than your file.

**Keys on your machine.** You do not need the API keys on your machine. When
a local command (`doctor` too) needs a key you have not set, it signs in as
the run account and reads it from Secret Manager. It reads only the keys that
command needs. Inside Cloud Run, the service and the job get their keys from
Secret Manager on their own.

**Your spreadsheet.** You own it, so Google lets you edit the tabs
leadscore protects (`Ranked`, `Health`, `Pushes`, ...) **with no warning**.
Do not edit the protected tabs. Type only in `Leads`, `Companies` and
`Overrides`, and change a lead through `Overrides` (or `leadscore
set-status`, `merge`, `mark-distinct`, `retry`). Editors other than you and
the service accounts cannot edit the protected tabs, and cannot share the
file further.

**Rotating a secret. [person]** Step 3 below prints the secret, so a person
runs this block, not an agent. On macOS, pipe step 3 to `pbcopy` so it never
shows. No webhook is refused at any point. Replace `P` with your project id:

```sh
# 1. Keep the current secret as the previous one, and add a new current one.
gcloud secrets versions access latest --secret receiver-secret --project P |
  gcloud secrets versions add receiver-secret-previous --project P --data-file=-
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' |
  gcloud secrets versions add receiver-secret --project P --data-file=-
# 2. Deploy: the receiver accepts both.
setup/gcp.sh redeploy
# 3. Paste the new secret into each Apollo workflow; read it with:
gcloud secrets versions access latest --secret receiver-secret --project P
# 4. Stop accepting the previous one: detach it first, then disable it.
setup/gcp.sh redeploy --finish-rotation
gcloud secrets versions disable latest --secret receiver-secret-previous --project P
```

Keep that order. A disabled version that is still attached stops a new
receiver instance from starting. To rotate an API key, set its variable
(`APOLLO_API_KEY` or `HUBSPOT_TOKEN`) and run `setup/gcp.sh secrets`. Runs
read the newest version. A key added for the first time after deploy needs
`setup/gcp.sh redeploy`, which the script reminds you of.

**Upgrading** is `setup/gcp.sh deploy <new image>`, then `leadscore doctor`.
Rolling back is deploying the previous image. A new version only adds tables
or columns, and an older version runs on a store a newer one extended.
Copying the spreadsheet first is still a good habit.

#### Run time and monthly cost

These are not measured yet, because measuring needs a billed project. The
live check (`LEADSCORE_LIVE_CLOUDRUN`) prints the run length, and the cost
follows from it. To fill in the table:

1. Run the live check, and note `MEASURE run length` for the synthetic Sheet.
   Then note the length of an ordinary run on a team-sized Sheet (2,000
   leads), from `gcloud run jobs executions list --job leadscore-run`.
2. Runs per month at the default 15 minutes: 4 × 24 × 30 = 2,880. The job has
   1 vCPU and 1 GiB, so a month uses 2,880 × run seconds vCPU-seconds and as
   many GiB-seconds.
3. Compare with Cloud Run's free tier per billing account (240,000
   vCPU-seconds and 450,000 GiB-seconds a month at the time of writing), and
   price the rest from Google's price list for the region. Add Cloud
   Scheduler (three jobs free per billing account), Secret Manager (a few
   secret versions and one access per run per secret) and the receiver. The
   receiver scales to zero and is billed only while it answers a webhook.

| Measure | Value |
|---|---|
| Run length, 2,000 leads | _to measure_ |
| Run length, live check's synthetic Sheet | _to measure_ |
| vCPU-seconds per month at 15 minutes | _to measure_ |
| Monthly cost beyond the free tier | _to measure_ |

### Path 3: Docker (a laptop or a server)

For technical users. One container runs the receiver and a run every
`schedule`. The store is SQLite on a named volume.

**Laptop or server?** A server with a domain gets live webhooks from Apollo
over HTTPS. A laptop has no public address, so pick one of two:

- **Poll** (the laptop example's default, `replies: polling`): replies are
  read from Apollo every six hours. Website visits come from Apollo's visitor
  CSV export, added as a CSV source with `events: true`. No extra account.
- **A Cloudflare Tunnel** (free) gives the laptop a public URL for live
  webhooks. Set `replies: receiver` and `receiver.public_url` to the tunnel's
  URL. **A tunnel only works while the laptop is awake.** Webhooks Apollo
  sends while it sleeps fail, and are lost unless Apollo retries them. The
  opt-out lookup before each push still keeps out a person who unsubscribed
  in HubSpot (see [Opt-outs](#opt-outs) for Apollo).

Steps:

1. **[agent]** Make a folder and copy in `compose.yaml` from this repository,
   the example `leadscore.yml` for your path, the example rubric as
   `rubric.yml`, and your CSV files. The example `leadscore.yml` is
   `examples/leadscore.laptop.yml` or `examples/leadscore.server.yml`, renamed
   `leadscore.yml` (or `examples/leadscore.csv-only.yml` for CSV only). The
   folder is mounted read-only at `/config`, and the export lists land in
   `./out`.
   - Make that folder private, because the lists hold personal data:
     `mkdir -p out && chmod 700 out`.
   - On macOS, keep the folder under your home folder. Colima shares only
     that with Docker, and a folder elsewhere shows up empty inside the
     container.
   - On Linux the container runs as its own user (uid 10001), so also run
     `sudo chown 10001:10001 out`, so the container can write the lists into
     `./out`. Then run `chmod o+rx . && chmod o+r leadscore.yml rubric.yml *.csv`,
     so the container can read your settings, rubric and leads (not
     secrets).
2. **[person]** Create `.env` in the folder, holding the keys (only those you
   use) and a receiver secret:

   ```sh
   APOLLO_API_KEY=...
   HUBSPOT_TOKEN=...
   LEADSCORE_RECEIVER_SECRET=...   # a long random value: openssl rand -hex 32
   LEADSCORE_IMAGE=asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:<tag>
   ```

   `LEADSCORE_IMAGE` names the private image, for before the first release
   only. Run `gcloud auth configure-docker asia-south1-docker.pkg.dev` first,
   and remove that line after the release. Keep `.env` private with
   `chmod 600 .env`. Docker reads it, the container does not. **Never commit
   `.env` (or a service-account key file) to git.**
3. **[person]** Write the rubric from your CSV headers and the example (see
   [The rubric](#the-rubric)). **[agent]** Check it with
   `leadscore rules check rubric.yml`, or, once the container is up,
   `docker compose exec leadscore leadscore rules check /config/rubric.yml`.
   In `leadscore.yml`, set `sinks.apollo.mailbox_id` (the sending mailbox's
   id or its email address), uncomment `sinks.hubspot` if you use HubSpot,
   and remove the blocks and the rubric lanes for tools you do not use.
4. Give the receiver an address (skip this for CSV only):
   - **A server. [person]** Point your domain's DNS at the server and open
     ports 80 and 443. **[agent]** In `compose.yaml`, replace
     `leads.example.com` in the Caddy service with your domain, and set
     `receiver.public_url: https://<your domain>` in `leadscore.yml`.
   - **A laptop that polls.** Nothing to do: keep `replies: polling`.
   - **A laptop with a tunnel. [person]** Start the tunnel to
     `localhost:8080`. **[agent]** Set `replies: receiver` and
     `receiver.public_url` to the tunnel's URL.
   - **A server or a tunnel: [person]** create the Apollo workflows from
     `setup/apollo/`, pointing at that address. A polling laptop with no
     tunnel gets no webhooks, so it skips the workflows.
5. **[agent]** Start it with `docker compose up -d` (on a server,
   `docker compose --profile caddy up -d`, so Caddy gets the certificate).
   It runs once at start, then every `schedule`. Pushes are off, so these
   runs only score.
6. **[agent]** If you use HubSpot, run
   `docker compose exec leadscore leadscore setup hubspot`. Then run
   `docker compose exec leadscore leadscore doctor` until it prints
   `0 failed`. Warnings are fine, but read each one.
7. **[agent]** See the results with
   `docker compose exec leadscore leadscore ranked --csv > ranked.csv`, and
   the export lists in `./out`. Those are owner-only files: on Linux, read
   them with `sudo cat out/<lane>.csv`, or
   `docker compose cp leadscore:/out/<lane>.csv .`.
8. **[agent]** Run `docker compose exec leadscore leadscore run --dry-run`
   and show the result to a person. It names leads by id, and
   `docker compose exec leadscore leadscore explain <lead id>` shows who each
   is. **[person]** Approve it, or change the rubric and repeat.
9. **[agent]** Only after that approval: set `pushes_enabled: true` in
   `leadscore.yml`. The next run picks it up, with no restart.

Every command runs inside the container:
`docker compose exec leadscore leadscore <command>`. `docker ps` shows the
container unhealthy when `/healthz` does: the last run failed, or none
succeeded in three `schedule` intervals. Each run logs one summary line
(`docker compose logs leadscore`). The compose file assumes the receiver's
port 8080 inside the container, so leave `receiver.port` unset.

**Upgrading** is changing the image tag in `compose.yaml` (or
`LEADSCORE_IMAGE`, which overrides it), `docker compose up -d`, then
`doctor`. The receiver is down for those few seconds, so webhooks sent then
rely on Apollo retrying. Rolling back is the previous tag. Copy the SQLite
volume first if you want a backup for a store damaged some other way.

#### A Google Sheet on Docker

A Docker install can keep a read-only Sheet view of its SQLite store, or use
a Sheet as the store itself. Both need a service account and its key file:

1. **[person]** Create a Google Cloud project (no billing needed for the
   view), and enable the Google Sheets and Google Drive APIs (and Cloud
   Storage for a Sheets store).
2. **[person]** Create one service account and a JSON key for it. Save the
   key in the folder as `sa-key.json`. On Linux, let the container read it
   and nobody else: `sudo chgrp 10001 sa-key.json && chmod 640 sa-key.json`.
   Never commit it to git.
3. **[agent]** In `leadscore.yml`, set `store.credentials: sa-key.json`. It is
   a path relative to the folder, so it works on your machine and in the
   container.

Then:

- **A read-only view of a SQLite store** (`Ranked`, `Health` and one tab
  per export lane, rewritten after every run). **[person]** Run
  `gcloud auth login --enable-gdrive-access`, then **[agent]** on your
  machine, in the folder, run `leadscore setup sheet --view`. It creates the
  spreadsheet with your login, shares it with the service account, and
  writes `store.view_spreadsheet`. Runs write it through the Sheets API with
  no lease, one tab at a time, so a failure can leave some tabs newer than
  others until the next run. A failed write, or a view that cannot be
  opened, shows in `Health` as a warning (`view_write_failed`,
  `sheet-access`) and the run stays healthy. **The view holds personal data
  (names, emails): share it only with named people, never by link.** The
  Workspace exception (Google Cloud step 6) and "Your spreadsheet" apply
  here too.
- **The store itself** (instead of SQLite): **[agent]** set
  `store: { type: sheets, credentials: sa-key.json }`. **[person]** Run
  `gcloud auth login --enable-gdrive-access`, then **[agent]**
  `leadscore setup sheet` (creates the spreadsheet, shares it with the
  service account, writes `store.spreadsheet`). **[person]** Create a Cloud
  Storage bucket for the run lease, give the service account Storage Object
  Admin on it, and **[agent]** set `store.lease_bucket` to its name. Do not
  run `setup/gcp.sh`: it is the Google Cloud path's script.

## Usage

### Export lists

An `export` lane in the rubric keeps a list instead of pushing to a tool.
Each lane gets one table (`Export <lane id>`, a tab on a Sheets store). On
SQLite it also gets one CSV in `export.dir` (`./out` on Docker), rewritten
after every run.

- A lead is listed once, the first run it matches. A run adds at most
  `ingest_chunk_rows` (2,000) new rows across all lists, and the rest follow
  in later runs. Every run refreshes each row's `status` and
  `do_not_contact`, also for lanes since removed from the rubric.
- **Filter on `do_not_contact` before every send.** It is `yes` for anyone
  opted out, blocked, at a company with an open deal, already contacted, or
  headed for a cold lane. Opt-outs reach the lists from the receiver,
  polling and `Overrides`. The Apollo and HubSpot opt-out lookups run only
  for leads about to be pushed.
- A cold lane claims its leads even before its sink is set up, so anyone it
  matches is `do_not_contact`. A CSV-only team removes the cold lanes from
  its rubric (`doctor` warns `cold_lane_no_sink:<lane>`).
- A cell that a spreadsheet would read as a formula (starting with `=`, `+`,
  `-` or `@`) is quoted with a leading `'` in the CSV files and in
  `ranked --csv` and `facts --csv`. So opening them in Excel or Sheets
  never runs a formula a lead's data carried.
- CSV files are written only for a SQLite store, with mode 0600, because they
  hold personal data. A plug-in store (for example Postgres,
  `docs/postgres-store.md`) keeps its lists as tables in that store.

### Health, status and doctor

- `leadscore status` prints the last run's result and every open problem,
  each with its fix.
- `leadscore doctor` prints one line per check, each problem with its fix.
  It exits 0 when nothing fails (warnings allowed) and 1 otherwise. It only
  reads: it never writes the store or takes the lease. The checks are listed
  in [docs/reference.md](docs/reference.md#doctor-checks).
- On Google Cloud, the `Health` tab's cell `H1` reads `STALE` (in red) when
  no run has succeeded in three `schedule` intervals.
- On Docker, `docker ps` shows the container unhealthy, as above.
- `SKILL.md` has a troubleshooting entry for every doctor check, and the loop
  for changing the rubric safely.

## Reference

[docs/reference.md](docs/reference.md) has the exact formats: the rubric,
`leadscore.yml`, the store tables, the receiver, the doctor checks, hosting
and the plug-in interfaces. This section covers what you need while setting
up.

### Commands

`leadscore help` lists every command.

- `leadscore run [--dry-run]` does one run. It reads `leadscore.yml` and the
  rubric fresh, and takes the run lease (if another run holds it, this one is
  skipped). It merges new input rows, `ingest_chunk_rows` per run, so a large
  first import finishes over several runs and nothing is pushed until it has.
  Then it scores every lead and saves `Ranked` and `Health`. It exits 1 when
  the run failed or finished unhealthy.
- With `--dry-run` it prints one line per lead whose verdict, status or
  planned lane would change, then totals. It takes no lease, writes nothing
  and spends nothing.
- `leadscore serve [--every [interval]]` is the receiver for Apollo webhooks
  and `/healthz`. With `--every` it also runs the loop: once at start, then
  each `interval` (or `schedule`, read at start) after the previous run
  ended, so runs never overlap. On SIGTERM it lets a running run save,
  stores every webhook it accepted, and exits.
- `leadscore doctor` and `leadscore status`: see
  [Health, status and doctor](#health-status-and-doctor).
- `leadscore ranked [--csv]` prints every lead's verdict, highest score first.
- `leadscore facts [--csv]` prints every company's stored facts (value,
  origin, the value a change replaced) and when Apollo was last asked, by
  domain. With `--csv`, it writes the `Company facts` table's own columns. It
  only reads.
- `leadscore explain <person>` prints one lead's verdict and the reasons
  behind it.
- `leadscore healthz` calls the local `/healthz` (the compose health check).
- `leadscore rules check <file>` compiles a rubric and lists every error.
- `leadscore config get <key>`, `config set-hosting k=v...` and `config push`
  read a value, write the `hosting` block, and upload the hosted bundle.
- `leadscore setup sheet [--view] [--repair]` and `leadscore setup hubspot`
  set up the spreadsheet and HubSpot.

The Overrides writers edit the `Overrides` table the same way on every
store. A person is an email, a LinkedIn URL or a lead id. A row for someone
not yet imported waits until they appear.

- `leadscore set-status <person> <status|none|resubscribe>` replaces the
  person's manual status (`replied_*`, `unsubscribed`, `blocked`), removes
  it, or undoes a manual `unsubscribed`.
- `leadscore merge <person> <person>` says the two are one person. The next
  run merges them for good.
- `leadscore mark-distinct <person> <person>` says two people who share a
  company and a name are different people.
- `leadscore retry [--lane <id>] [<person>]` retries failed pushes, for one
  person or everyone, in one lane or all.

### The rubric

Your ideal customer is one YAML file, the rubric. It says which columns
matter, how to label each company and person (tier, priority, ...), how to
score them, and where each lead goes. Start from `examples/rubric.yml`
(Apollo and HubSpot lanes) or `examples/rubric.csv-only.yml` (lists only).
The full format is in [`docs/reference.md`](docs/reference.md#the-rubric).

Check a rubric with `leadscore rules check rubric.yml`, which lists every
problem with its line. `SKILL.md` has the loop for changing it safely.

### Enrichment

With an `enrich` block, each run looks up company facts in Apollo for the
companies of your leads, using `APOLLO_API_KEY`. The facts are name,
headcount, funding stage, country and latest funding date.

```yaml
enrich: { type: apollo, max_age: 30d, max_lookups_per_run: 100, max_lookups_per_day: 400 }
```

- A company is looked up when it has never been, or when its last lookup (or
  Apollo's "not found") is older than `max_age`.
- Every call counts toward both budgets. So a large first import spreads over
  several days instead of spending a month of credits at once.
- Your `Companies` tab always wins over Apollo, and Apollo's value wins over
  a value from a lead sheet. When a value changes, the old one is kept in
  `Company facts` as `previous`.
- A dry run makes no lookups. A lookup that gets no answer waits a day, and
  three in a row stop enrichment for that run (the warning `enrich_failed`).
- The `apollo-key` check signs in with Apollo's free auth-health call on
  every run, so a bad key shows in `Health` without spending a credit.

### HubSpot

Add a `sinks.hubspot` block and set `HUBSPOT_TOKEN` to a private app's token:

```yaml
sinks:
  hubspot: { pipeline: Sales Pipeline, stage: Appointment scheduled }
```

`leadscore setup hubspot` creates the custom properties (`leadscore_lead_id`
and friends, in a `leadscore` group) and checks that the pipeline and stage
exist. Run it once, and again after changing `property_prefix`. It needs the
schema write scopes. Runs need contacts and deals read and write, companies
read, and schema read.

- A lane pushing to `hubspot:contacts` finds or creates the person's
  contact.
- A lane pushing to `hubspot:deals` also opens one deal per company (named by
  its domain), or adds the contact to the company's open deal.
- Before every push, leadscore reads HubSpot for the leads it may push. A
  contact that opted out of email makes the lead `unsubscribed`. A company
  with an open or won deal keeps its people out of cold lanes until the deal
  is closed lost.

### The receiver

`leadscore serve` accepts Apollo workflow requests at `POST /apollo/reply`
(sent, replies, opt-outs) and `POST /apollo/visit` (website visits), with
bodies from the templates in `setup/apollo/`. Each request carries the
receiver secret in the `X-Leadscore-Secret` header. When Apollo cannot set
headers, it can go in a top-level `leadscore_secret` body field instead,
which is removed before storing. Prefer the header, since a flood of slow
senders can delay body-secret requests but never header ones.

- The receiver answers 200 only once the event is stored. A wrong or missing
  secret gets 401. A request it could not store within 10 seconds gets 503,
  so Apollo can send it again. A repeated event is counted once.
- An opt-out or reply stored while a run is pushing still counts. Before
  each batch of 25 pushes, the run reads the newly stored events, so the
  person is not pushed in the next batch.
- Every opt-out and reply is kept in `Outcomes`, so it holds long after the
  90-day event window.

[docs/reference.md](docs/reference.md#receiver) lists every route and answer.

**Silence.** Reachable is not delivering. When the receiver is set up
(`receiver.public_url` is set), each run checks that every event kind it
expects arrived within `silence_threshold` (3 days by default). That means
`sent` with `replies: receiver`, and each `receiver.visit_events` kind. A
silent kind shows in `Health` as `silent:<kind>` and makes the run
unhealthy, so check that Apollo workflow. Without `receiver.public_url`,
silence is not watched, and `doctor` says so (`receivers:no_public_url`).
`doctor` also calls `<public_url>/healthz` to check Apollo can reach the
receiver.

**Receiver-only leads.** A lead known only from webhooks could be forged by
anyone with the secret. When a lane pushes one, `Health` shows the warning
`receiver_only_push:<lead>` until a source (a CSV or Sheet tab) reports that
person too. Cold lanes should require `receiver_only` to be false.

**Health check.** With the timer, `/healthz` is 200 while the last run
succeeded or none is due yet. It is 503 when the last run failed or none
succeeded in three intervals. Without the timer (Google Cloud) it is 200
unless storing events fails.

#### Keep the receiver secret private

The receiver secret (`LEADSCORE_RECEIVER_SECRET`) works like a password:
anyone who has it can send fake events, including a fake positive reply that
a deal lane would act on. Never paste it into chat, tickets or shared docs.
Type it only into `.env` (or Secret Manager) and the Apollo workflows, and
rotate it if it might have leaked. Without it every webhook is refused, while
`/healthz` and the timer keep working.

To rotate it on Docker, move the current value to
`LEADSCORE_RECEIVER_SECRET_PREVIOUS` in `.env` and set a new
`LEADSCORE_RECEIVER_SECRET`. Run `docker compose up -d`, update each Apollo
workflow, then remove the previous value and run `docker compose up -d`
again. Both are accepted in between, so no webhook is lost. On Google Cloud,
follow "Rotating a secret" under [Path 2](#path-2-google-cloud).

## Before you rely on it

These limits matter before you push to real people or send from a list.

### Not yet checked against live Apollo and HubSpot

The tests run against fake Apollo and HubSpot APIs. Some of the fakes' answers
were recorded from a real Apollo account, and the rest follow the vendors'
docs.

**Recorded from real Apollo calls (read only):**

- the sign-in check and a refused key
- the mailbox list and the sequence search
- the reply search: its date filter, paging and fields
- the error answer for an unknown contact

**Not yet checked against a real account:**

- **Apollo writes.** Creating a contact and adding it to a sequence (the
  call, its flags, re-adding, and the reasons Apollo skips a contact) follow
  Apollo's docs. No real answer was recorded.
- **Apollo details.** Whether the contact search spends credits, a real
  rate-limit (429) answer, and five of the eight documented reply labels.
- **Apollo workflow retries.** Whether Apollo resends a webhook that failed is
  unknown. If it does not, an event answered 503, or sent while the receiver
  was down or a laptop slept, is lost. Silence detection notices a workflow
  that goes quiet, not a single lost event.
- **HubSpot.** Every call. The HubSpot sink and its lookups are tested only
  against fakes built from HubSpot's docs.
- **Google Cloud.** The live check has not run on a billed project, so run
  time and monthly cost are not measured.

Before the first real push, review a dry run, and push to a small test
sequence or a test HubSpot pipeline first.

### Opt-outs

**Can contact someone who opted out (harmful):**

- **If you send only through Apollo:** leadscore does not yet know whether
  Apollo's contacts carry an opt-out flag it can read. Until that is
  confirmed, a person who clicked an unsubscribe link without replying may
  not be seen before a push. That is safe only if HubSpot is also a sink, or
  the receiver gets Apollo's `unsubscribed` webhook. The `apollo-key` check
  warns (`apollo-key:no_optout_flag`) until an unsubscribe webhook has been
  received.
- **Export lists** see opt-outs only from the receiver, polling and
  `Overrides`, as [Export lists](#export-lists) says.

**Blocks someone it could contact:** a lead at a company with an open or won
HubSpot deal stays out of cold lanes until the deal is closed lost. This is
by design.

### Monitoring and volume

- **No alerting in v1.** A failed run tells nobody. See
  [Choosing a path](#choosing-a-path) for what to watch.
- **Polled replies** are found by the date their email was sent. A reply that
  comes more than `sequence_length` plus `window_margin` (37 days by default)
  after its email is missed.
- **A Google Sheet holds 10 million cells.** At the design target (20,000
  leads, about 1,000 receiver events a day) a Sheets store uses about 6
  million. `doctor` warns at 70% (`sheets:cells`) and names the largest tabs.
  **Above about 1,500 events a day, use SQLite (Docker), or shorten
  `log_retention`.** An export tab holds up to 20,000 rows in that budget.
- **Apollo limits** on the account checked were 200 calls a minute, 400 an
  hour and 2,000 a day for each endpoint. The default enrichment budgets
  (100 lookups a run, 400 a day) stay inside them.

## Project

### Layout

| Path | What lives there |
|---|---|
| module root (package `leadscore`) | the public API: plug-in interfaces, types, errors, the adapter registry, `Run`, `Main` |
| `cmd/leadscore/` | the CLI binary |
| `internal/api` | the public types (re-exported by the root), the built-in header aliases |
| `internal/config` | loading `leadscore.yml`, `config get`, `config set-hosting` |
| `internal/check` | the `doctor` check framework and the checks |
| `internal/cli` | the commands, `doctor` among them |
| `internal/model` | the in-memory model of the store's tables |
| `internal/store/codec` | maps the model to table writes, loads a store and checks its schema version |
| `internal/store/sqlite` | the built-in SQLite store (WAL, lease row, event log) |
| `internal/store/sheets` | the built-in Google Sheets store (one batchUpdate per commit, monthly `Events` tabs, Cloud Storage lease file) and `setup sheet` |
| `internal/fakes/sheets`, `internal/fakes/gcs` | in-memory fakes of Google Sheets, Drive and Cloud Storage for tests |
| `internal/rules` | the rubric compiler and evaluator: YAML rules compiled to CEL |
| `internal/merge` | turns input rows into one lead per person: header aliases, identities, `same_as` merges, the Overrides tab |
| `internal/receiver` | `leadscore serve`: the Apollo webhook receiver, its write queue, `/healthz` and the Docker run timer |
| `internal/receiver/auth` | the constant-time secret check |
| `internal/engine` | the run: lease, sources and chunked merge, scoring, the two saves, `Ranked`, `Health`, lanes and the ledger, export lists and their CSVs, the Sheet view |
| `internal/logredact` | log redaction: logs carry ids, never emails |
| `internal/csvsafe` | quotes CSV cells a spreadsheet would read as formulas |
| `internal/hosting` | Google Cloud: the fixed resource names, Secret Manager reads and writes (`config push`, hosted keys), the schedule's cron form, and the reads behind the `hosting` check |
| `internal/fakes/gcp` | an in-memory fake of Secret Manager, Cloud Run, Cloud Scheduler and Artifact Registry for tests |
| `adapters/csv` | the CSV file source (`type: csv`): lead rows, or event rows with `events: true` |
| `adapters/sheetsource` | the Google Sheet tab source (`type: sheetsource`): tabs of the team's spreadsheet |
| `adapters/apollo` | the Apollo client, enrichment, sequences sink, opt-out lookup and reply poller |
| `adapters/hubspot` | the HubSpot sink (`hubspot:contacts`, `hubspot:deals`), the opt-out and deal lookup, `setup hubspot` and the `hubspot` check |
| `internal/fakes/hubspot`, `internal/fakes/apollo` | fake vendor APIs for tests, held to the fixtures in `testdata/vendors` |
| `storetest/`, `sinktest/` | conformance suites for plug-in stores and sinks |
| `examples/` | made-up example rubrics (`rubric.yml`, and `rubric.csv-only.yml` with export lanes only), a sample lead sheet (`leads.csv`), and example `leadscore.yml` files for CSV only, Docker on a laptop and on a server, and Google Cloud |
| `compose.yaml`, `Dockerfile` | the Docker setup: the image and the compose file that runs it |
| `setup/apollo/` | the Apollo workflow templates the receiver accepts |
| `setup/gcp.sh` | the Google Cloud setup script, one subcommand per runbook step |
| `SKILL.md` | for agents running an install: the rule-change loop and one troubleshooting entry per doctor check |
| `docs/reference.md` | the exact formats: rubric, `leadscore.yml`, store tables, receiver, doctor checks, hosting, plug-in interfaces |
| `docs/postgres-store.md` | how to write a Postgres store as a plug-in |
| `docs/adr/` | the decision records behind the receiver, the two-phase saves and the push ledger |
| `docs/design/` | the design notes behind it (not shipped in releases) |

### How it ships

Each release is a `v*` tag. The release workflow tests the tagged commit,
then publishes two things:

- `leadscore` binaries for Linux, macOS and Windows on amd64 and arm64, with
  a `checksums.txt`, as a GitHub release.
- The container image `ghcr.io/tetriz-ai/leadscore:<version>` for
  linux/amd64 and linux/arm64. A final version also moves `latest`, and a
  pre-release tag such as `v0.2.0-rc1` does not.

### Versioning and compatibility

Releases follow semantic versioning, starting at v0.1.0. Before v1, a minor
release may break the `leadscore.yml` keys, the rubric format or the
commands. [CHANGELOG.md](CHANGELOG.md) lists every change by release.

- The plug-in interfaces never gain methods after v0.1.0. A new capability
  is a separate optional interface.
- A new version changes the store only by adding tables or columns. An older
  version runs on a store a newer minor version extended, and only a newer
  major version is refused.
- Go 1.25 is the floor, and CI tests on Go 1.25.

### Tests

The suite covers the rubric compiler, merging, every store against the
conformance suites, the sinks against fake Apollo and HubSpot APIs, the
receiver, the doctor checks and the commands. It needs no key or account.

```sh
go build ./...
go vet ./...
go test -race ./...
```

CI also runs `setup/gcp.sh` against a fake `gcloud` (`testdata/gcloud/gcloud`),
builds the image for both architectures, and runs `docker compose up` with a
timer run end to end (`testdata/compose/smoke.sh`).

The live checks talk to real services and are skipped unless their variable
is set:

| Variable | What it checks |
|---|---|
| `LEADSCORE_LIVE_APOLLO` | Read-only calls against a real Apollo account. It names an env file holding `APOLLO_API_KEY` and `APOLLO_MAILBOX_ID`. It never writes, and keeps raw answers outside the repository. |
| `LEADSCORE_LIVE_SHEETS` | Saves 20,000 leads and a year of events to a scratch spreadsheet and loads them within a minute. Set it to `1` or the path of a service-account key file. Your own gcloud login (or the account `LEADSCORE_LIVE_SHEETS_OWNER` names) creates the spreadsheet, so it needs `gcloud auth login --enable-gdrive-access`. |
| `LEADSCORE_LIVE_CLOUDRUN` | Sets up a billed, throwaway project (`LEADSCORE_LIVE_PROJECT`, never one a real install uses; it refuses any project listed in `LEADSCORE_LIVE_DENY_PROJECTS`) with `setup/gcp.sh` and the image in `LEADSCORE_LIVE_IMAGE`. It starts a run longer than 3 minutes from Cloud Scheduler, posts webhook bursts during the run and during a redeploy, and checks no event was lost. Its comment lists what to do first. |

### Contributing, security and license

- Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) and the
  [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
- Report a vulnerability privately through the repo's [Security tab](https://github.com/HarshitBadhwar8/leadscore/security).
- Licensed under the [MIT License](LICENSE).

Copyright 2026 Workloom Solutions Private Limited.
