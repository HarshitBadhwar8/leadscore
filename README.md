# leadscore

An open-source outbound engine. It brings leads in (CSV files, Google Sheet
tabs, Apollo webhooks), merges them into people, scores them with your own
YAML rubric, routes each lead to one lane, and pushes it to Apollo sequences,
HubSpot or an export list. It reads replies and opt-outs back, so nobody who
replied or opted out gets cold outreach again.

This README is the setup runbook. Follow one path from top to bottom. A
coding agent can follow it too: each step says who does it.

- **[agent]**: the agent (or you) runs it.
- **[person]**: a person must do it: paste a key, approve billing, create an
  account, or say yes to a dry run. An agent stops and asks.

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
  person (opted out, already contacted, in a deal, or claimed by a cold
  lane).
- **Receiver:** the always-on web address Apollo sends replies, opt-outs and
  website visits to (`leadscore serve`).

## Pick your path

| Path | For | Store | Runs on |
|---|---|---|---|
| **Google Cloud** | non-technical teams: nothing runs on your machine after setup | a Google Sheet | Cloud Run and Cloud Scheduler, in your own Google Cloud project |
| **Docker on a server** | technical users with a VM and a domain | SQLite (optionally a Sheet view) | your server, with Caddy for HTTPS |
| **Docker on a laptop** | technical users trying it out, or a small team | SQLite | your machine, while it is awake |
| **CSV only** | a first try, or a team that only wants scored lists | SQLite | Docker or the plain binary |

Until the first release, every path needs someone comfortable with a
terminal: the CLI is built from source, and Google Cloud setup runs
`gcloud` and `setup/gcp.sh` commands. After release, a non-technical person
can follow Path 2 with a coding agent.

Not sure? Start with **CSV only**: ten minutes, no accounts, and you see your
leads ranked. Every other path builds on it.

On every path, runs start with **pushes off**: leadscore scores and lists,
you review a dry run, and only then turn pushing on.

**There is no alerting in v1, on any path.** Nobody is emailed or messaged
when a run fails. Someone has to look (`leadscore status`, the `Health` tab,
`docker ps`), or point an uptime monitor at the receiver's `/healthz`.

## Before you start

**[person]** Have ready, for the path you picked:

- **Every path:** your leads as a CSV file (or a Google Sheet tab) with a
  header row. Columns are matched by name: `Email`, `Full Name`, `Job Title`,
  `Company`, `Website`, `LinkedIn` and their usual spellings work as they
  are.
- **Apollo or HubSpot:** a paid Apollo plan (workflows and the API need it)
  and an Apollo master API key; a HubSpot private app token. Not needed for
  CSV only.
- **Docker paths:** Docker (Docker Desktop or Colima on macOS, Docker Engine
  on Linux) or podman with compose.
- **Docker on a server:** a VM with a public IP, ports 80 and 443 open, and a
  domain (or subdomain) whose DNS points at it.
- **Google Cloud:** a Google account that may create a project with billing,
  and `gcloud` installed and logged in (`gcloud auth login`).

### Install the CLI

The Docker paths need no CLI on your machine: every command runs inside the
container. You need it for **CSV only as a plain binary**, for **Google
Cloud**, and to make a Google Sheet (`setup sheet`) on any path.

- **[agent]** After the first release: download the binary for your OS from
  the repository's releases page and put it on your `PATH` as `leadscore`.
- **[agent]** Before the first release (now): build it from the private
  repository, with Go 1.25 or newer:

  ```sh
  git clone https://github.com/HarshitBadhwar8/leadscore.git
  cd leadscore && go build -o ~/.local/bin/leadscore ./cmd/leadscore
  leadscore help    # check: ~/.local/bin (or the folder you chose) must be on your PATH
  ```

  The pre-release container image also works for most commands, but not for
  `setup sheet` (it signs in with your machine's `gcloud`) or
  `setup/gcp.sh` (it calls `leadscore` on your `PATH`):

  ```sh
  docker run --rm -v ~/.config/gcloud:/home/leadscore/.config/gcloud:ro -v "$PWD":/config \
    asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:<tag> doctor
  ```

Every command takes `--config <file>` and `--rubric <file>`. Without
`--config`, commands read `/config/bundle.yaml`, else `/config/leadscore.yml`,
else `./leadscore.yml`, so run them from the folder holding `leadscore.yml`.

## Path 1: CSV only

CSV files in, scored lists out. No vendor accounts.

1. **[agent]** Make a folder for the install and copy in, from this
   repository's `examples/`: `leadscore.csv-only.yml` as `leadscore.yml`,
   `rubric.csv-only.yml` as `rubric.yml`, and your CSV as `leads.csv` (or
   `examples/leads.csv` to try it with made-up leads).
2. **[person]** Write the rubric for your ideal customer: start from the
   example, and see "The rubric" below. Check it:
   `leadscore rules check rubric.yml`. The example rubric has export lanes
   only, on purpose: a cold lane claims the leads it matches even before
   Apollo or HubSpot is set up, and they show `do_not_contact: yes` on every
   list.
3. Run it, as a plain binary or on Docker:
   - **Plain binary. [agent]** In `leadscore.yml`, change `store.path` to
     `leadscore.db` and `export.dir` to `out` (paths are relative to the
     folder). Set a receiver secret (doctor checks it is set, even though a
     CSV-only install receives no webhooks), and keep it for new terminals
     by adding the line to your shell profile (`~/.zshrc` or `~/.bashrc`):
     `export LEADSCORE_RECEIVER_SECRET=<the output of openssl rand -hex 32>`.
     Then
     `leadscore run --dry-run` (what a run would do, writing nothing; it
     names leads by id, and after the first real run
     `leadscore explain <lead id>` shows who each is), and `leadscore run`.
   - **Docker. [agent]** Follow Path 3 below with these files; skip its
     Apollo and HubSpot steps.
4. **[agent]** `leadscore doctor` until it prints `0 failed` (on Docker,
   `docker compose exec leadscore leadscore doctor`). With CSV only, expect
   two warnings: `pushes-enabled:off` and `receivers:no_public_url`.
5. **[agent]** See the results: `leadscore ranked` (every lead, best first),
   `leadscore explain <email>` (why a lead scored as it did), and one CSV per
   export lane in `out/` (`out/call-list.csv`, `out/nurture.csv` with the
   example rubric). **Filter on `do_not_contact` before every send.**

Run `leadscore run` again whenever your CSV changes (or let Docker run it
every 15 minutes). A lead is added to a list once; each run refreshes its
`status` and `do_not_contact`.

## Path 2: Google Cloud

The receiver runs as a Cloud Run service, each run as a Cloud Run job that
Cloud Scheduler starts, and the store is a Google Sheet you own. Run every
step from one folder on your machine (macOS, Linux or Google Cloud Shell),
with the CLI installed. `setup/gcp.sh` is in this repository; it changes
`leadscore.yml` only through `leadscore config set-hosting`. Add `--dry-run`
to any `setup/gcp.sh` step to print what it would change without changing it.

1. **[person]** Prerequisites: a paid Apollo plan, `gcloud` installed and
   logged in as someone who may create projects, service accounts, Cloud Run
   services and jobs, Cloud Scheduler jobs and Artifact Registry
   repositories, and grant roles on them.
2. **[agent]** Install the CLI (above). Copy `examples/leadscore.gcp.yml` as
   `leadscore.yml` and `examples/rubric.yml` as `rubric.yml` into an empty
   folder, and `setup/gcp.sh` beside them (or call it by its path).
3. **[person]** Create or pick a Google Cloud project, and turn billing on
   for it (Google asks a person to approve billing).
4. **[agent]** `setup/gcp.sh accounts --project <id> [--region asia-south1]`:
   enables the APIs (Cloud Run, Cloud Scheduler, Secret Manager, Cloud
   Storage, Sheets, Drive, Artifact Registry, IAM Service Account
   Credentials), creates the run account (`leadscore-run`) and the receiver
   account (`leadscore-receiver`), writes the `hosting` block, and ends by
   signing your local commands in as the run account. **[person]** A browser
   opens: sign in. From then on, Google's application-default login on your
   machine acts as the run account for every program that uses it;
   `gcloud auth application-default revoke` undoes it.
5. **[agent]** `setup/gcp.sh bucket`: creates the lease bucket,
   `<project>-leadscore-lease`. Bucket names are global: only if that name is
   taken, set `store.lease_bucket` to another and run it again.
6. **[person]** `gcloud auth login --enable-gdrive-access` (a browser opens),
   then **[agent]** `leadscore setup sheet`: creates the spreadsheet with
   your own login, so you own it, shares it with both accounts, and writes
   `store.spreadsheet`. If a Google Workspace sharing policy blocks the
   share, the command says so: **[person]** ask your Workspace admin to allow
   sharing this file with the service accounts (an exception for
   `gserviceaccount.com`), then run `leadscore setup sheet --repair`.
7. **[agent]** `setup/gcp.sh secrets`: creates the secrets. **[person]** It
   asks for each API key at a hidden prompt; paste it (Enter skips a key you
   do not use). It generates the receiver secret and never prints it (see
   "Keep the receiver secret private").
8. **[person]** Write the rubric (see "The rubric"), and put your leads in
   the spreadsheet's `Leads` tab. **[agent]** If you use HubSpot, add the
   `sinks.hubspot` block and run `leadscore setup hubspot`. Then
   `leadscore config push`: uploads `leadscore.yml` and the rubric together
   as one version of the `leadscore-config` secret. It refuses a rubric that
   does not compile, a SQLite store or CSV path (Cloud Run keeps no files), a
   `schedule` or `deadline` Cloud Scheduler cannot run (write them like
   `15m`), and anything that looks like a key, including the API keys
   already in Secret Manager. It cannot see the receiver secret, so never
   paste that into `leadscore.yml` or the rubric.
9. **[agent]** `setup/gcp.sh deploy <image>`: the receiver service (at most
   one instance, no sign-in check so Apollo can reach it) and the run job
   (task timeout the deadline plus 90 seconds, no retries). Before the first
   release, `<image>` is the private registry's
   `asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:<tag>`, and
   **[person]** a `leadscore-dev` owner grants your project's Cloud Run
   service agent Artifact Registry Reader on that repository. A release image
   (`ghcr.io/tetriz-ai/leadscore:<version>`) is pulled through an Artifact
   Registry repository, `ghcr-proxy`, that this step creates. It prints the
   receiver's address: **[agent]** set `receiver.public_url` to it and run
   `leadscore config push` again.
10. **[person]** Create the Apollo workflows from the templates in
    `setup/apollo/` (its README says how), pointing at the receiver's
    address. Read the secret only when you paste it into Apollo:
    `gcloud secrets versions access latest --secret receiver-secret`.
11. **[agent]** `setup/gcp.sh schedule`: creates the scheduler account and the
    scheduler job from `schedule` (UTC). Pushes are still off, so these runs
    only score.
12. **[agent]** `leadscore doctor` until it prints `0 failed` (warnings are
    fine; read each). Then open the spreadsheet's `Ranked` tab.
13. **[agent]** `leadscore run --dry-run` and show the result to a person
    (it names leads by id; `leadscore explain <lead id>` shows who each is).
    **[person]** Approve it, or change the rubric and repeat.
14. **[agent]** Only after that approval: set `pushes_enabled: true` and run
    `leadscore config push`. The next run pushes.

**Changing settings or rules later** is editing the files and
`leadscore config push`; the next run reads them, with no redeploy (SKILL.md
has the full loop). A new `schedule` also needs `setup/gcp.sh schedule`, and
a new `deadline` `setup/gcp.sh redeploy`. Each run records the bundle version
it read in `State` (`config_version`), and `doctor`'s `rubric-version` line
warns when the last run used another rubric than your file.

**Keys on your machine.** You do not need the API keys on your machine. When
a local command (`doctor` too) needs a key you have not set, it reads it
from Secret Manager. It signs in as the run account to do that. It reads
only the keys that command needs. Inside Cloud Run, the service and the job
get their keys from Secret Manager on their own.

**Your spreadsheet.** You own it, so Google lets you edit the tabs
leadscore protects (`Ranked`, `Health`, `Pushes`, ...) **with no warning**.
Do not edit the protected tabs. Type only in `Leads`, `Companies` and
`Overrides`; change a lead through `Overrides` (or `leadscore set-status`,
`merge`, `mark-distinct`, `retry`). Editors other than you and the service accounts cannot edit the
protected tabs, and cannot share the file further.

**Rotating a secret. [person]** Step 3 prints the secret, so a person runs
this block, not an agent (or pipe step 3 to `pbcopy` on macOS so it never
shows). No webhook is refused at any point. Replace `P` with your project
id:

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

Keep that order: a disabled version that is still attached stops a new
receiver instance from starting. To rotate an API key, set its variable
(`APOLLO_API_KEY` or `HUBSPOT_TOKEN`) and run `setup/gcp.sh secrets`; runs
read the newest version. A key added for the first time after deploy needs
`setup/gcp.sh redeploy`, which the script reminds you of.

**Upgrading** is `setup/gcp.sh deploy <new image>`, then `leadscore doctor`;
rolling back is deploying the previous image. A new version only adds tables
or columns, and an older version runs on a store a newer one extended.
Copying the spreadsheet first is still a good habit.

### Run time and monthly cost

Not measured yet: this needs a billed project. The live check
(`LEADSCORE_LIVE_CLOUDRUN`) prints the run length; the cost follows from it.
The method, to fill in the table:

1. Run the live check, and note `MEASURE run length` for the synthetic Sheet,
   then the length of an ordinary run on a team-sized Sheet (2,000 leads),
   from `gcloud run jobs executions list --job leadscore-run`.
2. Runs per month at the default 15 minutes: 4 × 24 × 30 = 2,880. The job has
   1 vCPU and 1 GiB, so a month uses 2,880 × run seconds vCPU-seconds and as
   many GiB-seconds.
3. Compare with Cloud Run's free tier per billing account (240,000
   vCPU-seconds and 450,000 GiB-seconds a month at the time of writing) and
   price the rest from Google's price list for the region. Add Cloud
   Scheduler (three jobs free per billing account), Secret Manager (a few
   secret versions and one access per run per secret) and the receiver
   (scales to zero; billed only while it answers a webhook).

| Measure | Value |
|---|---|
| Run length, 2,000 leads | _to measure_ |
| Run length, live check's synthetic Sheet | _to measure_ |
| vCPU-seconds per month at 15 minutes | _to measure_ |
| Monthly cost beyond the free tier | _to measure_ |

## Path 3: Docker (a laptop or a server)

For technical users. One container runs the receiver and a run every
`schedule`; the store is SQLite on a named volume.

**Laptop or server?** A server with a domain gets live webhooks from Apollo
over HTTPS. A laptop has no public address, so pick one of two:

- **Poll** (the laptop example's default, `replies: polling`): replies are
  read from Apollo every six hours, and website visits come from Apollo's
  visitor CSV export as a CSV source with `events: true`. No extra account.
- **A Cloudflare Tunnel** (free) gives the laptop a public URL for live
  webhooks: set `replies: receiver` and `receiver.public_url` to the tunnel's
  URL. **A tunnel only works while the laptop is awake**: webhooks Apollo
  sends while it sleeps fail, and are lost unless Apollo retries them. The
  opt-out lookup before each push (HubSpot's; Apollo's, see "Enrichment")
  still keeps a person who unsubscribed there out.

Steps:

1. **[agent]** Make a folder and copy in `compose.yaml` from this repository,
   the example `leadscore.yml` for your path (`examples/leadscore.laptop.yml`
   or `examples/leadscore.server.yml`, renamed `leadscore.yml`; or
   `examples/leadscore.csv-only.yml` for CSV only), the example rubric as
   `rubric.yml`, and your CSV files. The folder is mounted read-only at
   `/config`; the export lists land in `./out`. Make that folder private
   (the lists hold personal data): `mkdir -p out && chmod 700 out`. On
   macOS, keep the folder under your home folder: Colima shares only that
   with Docker, and a folder elsewhere shows up empty inside the container.
   On Linux the container runs as its own user (uid 10001), so also run:
   - `sudo chown 10001:10001 out`: the container can write the lists into
     `./out`.
   - `chmod o+rx . && chmod o+r leadscore.yml rubric.yml *.csv`: the
     container can read your settings, rubric and leads (not secrets).
2. **[person]** Create `.env` in the folder, holding the keys (only those you
   use) and a receiver secret:

   ```sh
   APOLLO_API_KEY=...
   HUBSPOT_TOKEN=...
   LEADSCORE_RECEIVER_SECRET=...   # a long random value: openssl rand -hex 32
   LEADSCORE_IMAGE=asia-south1-docker.pkg.dev/leadscore-dev/leadscore/leadscore:<tag>
   ```

   `LEADSCORE_IMAGE` is for before the first release only (the private
   image; `gcloud auth configure-docker asia-south1-docker.pkg.dev` first).
   Remove that line after the release. Keep `.env` private with
   `chmod 600 .env`: Docker reads it, the container does not. **Never commit
   `.env` (or a service-account key file) to git.**
3. **[person]** Write the rubric from your CSV headers and the example (see
   "The rubric"); **[agent]** check it with `leadscore rules check rubric.yml`
   (or, once the container is up,
   `docker compose exec leadscore leadscore rules check /config/rubric.yml`).
   In `leadscore.yml`, set `sinks.apollo.mailbox_id`, uncomment
   `sinks.hubspot` if you use HubSpot, and remove the blocks and the rubric
   lanes for tools you do not use.
4. Give the receiver an address (skip this for CSV only):
   - **A server. [person]** Point your domain's DNS at the server and open
     ports 80 and 443. **[agent]** In `compose.yaml`, replace
     `leads.example.com` in the Caddy service with your domain, and set
     `receiver.public_url: https://<your domain>` in `leadscore.yml`.
   - **A laptop that polls.** Nothing to do: keep `replies: polling`.
   - **A laptop with a tunnel. [person]** Start the tunnel to
     `localhost:8080`; **[agent]** set `replies: receiver` and
     `receiver.public_url` to the tunnel's URL.
   - **A server or a tunnel: [person]** create the Apollo workflows from
     `setup/apollo/`, pointing at that address. A polling laptop with no
     tunnel gets no webhooks, so it skips the workflows.
5. **[agent]** Start it: `docker compose up -d` (a server:
   `docker compose --profile caddy up -d`, so Caddy gets the certificate).
   It runs once at start, then every `schedule`. Pushes are off, so these
   runs only score.
6. **[agent]** If you use HubSpot,
   `docker compose exec leadscore leadscore setup hubspot`. Then
   `docker compose exec leadscore leadscore doctor` until it prints
   `0 failed` (warnings are fine; read each).
7. **[agent]** See the results:
   `docker compose exec leadscore leadscore ranked --csv > ranked.csv`, and
   the export lists in `./out` (owner-only files: on Linux,
   `sudo cat out/<lane>.csv`, or
   `docker compose cp leadscore:/out/<lane>.csv .`).
8. **[agent]** `docker compose exec leadscore leadscore run --dry-run` and
   show the result to a person (it names leads by id;
   `docker compose exec leadscore leadscore explain <lead id>` shows who). **[person]** Approve it, or change the rubric
   and repeat.
9. **[agent]** Only after that approval: set `pushes_enabled: true` in
   `leadscore.yml`. The next run picks it up, with no restart.

Every command runs inside the container:
`docker compose exec leadscore leadscore <command>`. `docker ps` shows the
container unhealthy when `/healthz` does: the last run failed, or none
succeeded in three `schedule` intervals. Each run logs one summary line
(`docker compose logs leadscore`). The compose file assumes the receiver's
port 8080 inside the container; leave `receiver.port` unset.

**Upgrading** is changing the image tag in `compose.yaml` (or
`LEADSCORE_IMAGE`, which overrides it), `docker compose up -d`, then
`doctor`. The receiver is down for those few seconds, so webhooks sent then
rely on Apollo retrying. Rolling back is the previous tag. Copy the SQLite
volume first if you want a backup for a store damaged some other way.

### A Google Sheet on Docker

Both need a service account and its key file:

1. **[person]** Create a Google Cloud project (no billing needed for the
   view), and enable the Google Sheets and Google Drive APIs (and Cloud
   Storage for a Sheets store).
2. **[person]** Create one service account and a JSON key for it. Save the
   key in the folder as `sa-key.json`. On Linux, let the container read it
   and nobody else: `sudo chgrp 10001 sa-key.json && chmod 640 sa-key.json`.
   Never commit it to git.
3. **[agent]** In `leadscore.yml`, set `store.credentials: sa-key.json` (a
   path relative to the folder, so it works on your machine and in the
   container).

Then:

- **A read-only view of a SQLite store** (`Ranked`, `Health` and one tab
  per export lane, rewritten after every run). **[person]**
  `gcloud auth login --enable-gdrive-access`, then **[agent]** on your
  machine, in the folder: `leadscore setup sheet --view`. It creates the
  spreadsheet with your login, shares it with the service account, and
  writes `store.view_spreadsheet`. Runs write it through the Sheets API with
  no lease, one tab at a time (a failure can leave some tabs newer than
  others until the next run). A failed write, or a view that cannot be
  opened, shows in `Health` as a warning (`view_write_failed`,
  `sheet-access`) and the run stays healthy. **The view holds personal data
  (names, emails): share it only with named people, never by link.** The
  Workspace exception (Google Cloud step 6) and "Your spreadsheet" apply
  here too.
- **The store itself** (instead of SQLite): **[agent]** set
  `store: { type: sheets, credentials: sa-key.json }`; **[person]**
  `gcloud auth login --enable-gdrive-access`, then **[agent]**
  `leadscore setup sheet` (creates the spreadsheet, shares it with the
  service account, writes `store.spreadsheet`). **[person]** Create a Cloud
  Storage bucket for the run lease, give the service account Storage Object
  Admin on it, and **[agent]** set `store.lease_bucket` to its name. Do not
  run `setup/gcp.sh`: it is the Google Cloud path's script.

## Using it

### Export lists

An `export` lane in the rubric keeps a list instead of pushing to a tool:
one table per lane (`Export <lane id>`, a tab on a Sheets store) and, on
SQLite, one CSV per lane in `export.dir` (`./out` on Docker), rewritten after
every run.

- A lead is listed once, the first run it matches. A run adds at most
  `ingest_chunk_rows` (2,000) new rows across all lists; the rest follow in
  later runs. Every run refreshes each row's `status` and `do_not_contact`,
  also for lanes since removed from the rubric.
- **Filter on `do_not_contact` before every send.** It is `yes` for anyone
  opted out, blocked, at a company with an open deal, already contacted, or
  headed for a cold lane. Opt-outs reach the lists from the receiver,
  polling and `Overrides`; the Apollo and HubSpot opt-out lookups run only
  for leads about to be pushed.
- A cold lane claims its leads even before its sink is set up: anyone it
  matches is `do_not_contact`. So a CSV-only team removes the cold lanes
  from its rubric (`doctor` warns `cold_lane_no_sink:<lane>`).
- A cell that a spreadsheet would read as a formula (starting with `=`, `+`,
  `-` or `@`) is quoted with a leading `'` in the CSV files and in
  `ranked --csv` and `facts --csv`, so opening them in Excel or Sheets
  never runs a formula a lead's data carried.
- CSV files are written only for a SQLite store, mode 0600 (they hold
  personal data). A plug-in store (for example Postgres,
  `docs/postgres-store.md`) keeps its lists as tables in that store.

### Health, status and doctor

- `leadscore status`: the last run's result and every open problem, each with
  its fix.
- `leadscore doctor`: one line per check (`docs/reference.md`), each
  problem with its fix; exits 0 when nothing fails (warnings allowed) and 1
  otherwise. It only reads: it never writes the store or takes the lease.
- Google Cloud: the `Health` tab's cell `H1` reads `STALE` (in red) when no
  run has succeeded in three `schedule` intervals.
- Docker: `docker ps` shows the container unhealthy, as above.
- **No alerting in v1:** see "Pick your path". `SKILL.md` has a
  troubleshooting entry for every doctor check.

### Volume limits

A Google Sheet holds 10 million cells. At the design target (20,000 leads,
about 1,000 receiver events a day) a Sheets store uses about 6 million;
`doctor` warns at 70% (`sheets:cells`) and names the largest tabs. **Above
about 1,500 events a day, use SQLite (Docker), or shorten `log_retention`.**
An export tab holds up to 20,000 rows in that budget.

## Reference

### Commands

`leadscore help` lists every command.

- `leadscore run [--dry-run]`: one run. It reads `leadscore.yml` and the
  rubric fresh, takes the run lease (another run holding it means this one
  is skipped), merges new input rows (`ingest_chunk_rows` per run; a large
  first import finishes over several runs, and nothing is pushed until it
  has), scores every lead, and saves `Ranked` and `Health`. It exits 1 when
  the run failed or finished unhealthy. With `--dry-run` it prints one line
  per lead whose verdict, status or planned lane would change, then totals;
  it takes no lease, writes nothing and spends nothing.
- `leadscore serve [--every [interval]]`: the receiver for Apollo webhooks and
  `/healthz`. With `--every` it also runs the loop: once at start, then each
  `interval` (or `schedule`, read at start) after the previous run ended, so
  runs never overlap. On SIGTERM it lets a running run save, stores every
  webhook it accepted, and exits.
- `leadscore doctor`, `leadscore status`: see "Health, status and doctor".
- `leadscore ranked [--csv]`: every lead's verdict, highest score first.
- `leadscore facts [--csv]`: every company's stored facts (value, origin,
  the value a change replaced) and when Apollo was last asked, by domain.
  With `--csv`, the `Company facts` table's own columns. It only reads.
- `leadscore explain <person>`: one lead's verdict and the reasons behind it.
- `leadscore healthz`: calls the local `/healthz` (the compose health check).
- `leadscore rules check <file>`: compile a rubric and list every error.
- `leadscore config get <key>`, `config set-hosting k=v...`, `config push`:
  read a value, write the `hosting` block, upload the hosted bundle.
- `leadscore setup sheet [--view] [--repair]`, `leadscore setup hubspot`.

The Overrides writers edit the `Overrides` table the same way on every
store. A person is an email, a LinkedIn URL or a lead id; a row for someone
not yet imported waits until they appear.

- `leadscore set-status <person> <status|none|resubscribe>`: replace the
  person's manual status (`replied_*`, `unsubscribed`, `blocked`), remove it,
  or undo a manual `unsubscribed`.
- `leadscore merge <person> <person>`: the two are one person; the next run
  merges them for good.
- `leadscore mark-distinct <person> <person>`: two people who share a company
  and a name are different people.
- `leadscore retry [--lane <id>] [<person>]`: retry failed pushes, for one
  person or everyone, in one lane or all.

### The rubric

Your ideal customer is one YAML file, the rubric: which columns matter, how
to label each company and person (tier, priority, ...), how to score them,
and where each lead goes. Start from `examples/rubric.yml` (Apollo and
HubSpot lanes) or `examples/rubric.csv-only.yml` (lists only); the full
format is in [`docs/reference.md`](docs/reference.md#the-rubric). Check
a rubric with `leadscore rules check rubric.yml`, which lists every problem
with its line. `SKILL.md` has the loop for changing it safely.

### Enrichment

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
changes, the old one is kept in `Company facts` as `previous`. A dry run
makes no lookups. A lookup that gets no answer waits a day, and three in a
row stop enrichment for that run (the warning `enrich_failed`). The
`apollo-key` check signs in with Apollo's free auth-health call on every run,
so a bad key shows in `Health` without spending a credit.

**If you send only through Apollo:** leadscore does not yet know whether
Apollo's contacts carry an opt-out flag it can read. Until that is
confirmed, a person who clicked an unsubscribe link without replying may not
be seen before a push, unless HubSpot is also a sink or the receiver gets
Apollo's `unsubscribed` webhook. The `apollo-key` check warns
(`apollo-key:no_optout_flag`) until an unsubscribe webhook has been received.

### HubSpot

Add a `sinks.hubspot` block and set `HUBSPOT_TOKEN` to a private app's token:

```yaml
sinks:
  hubspot: { pipeline: Sales Pipeline, stage: Appointment scheduled }
```

`leadscore setup hubspot` creates the custom properties (`leadscore_lead_id`
and friends, in a `leadscore` group) and checks that the pipeline and stage
exist; run it once, and again after changing `property_prefix`. It needs the
schema write scopes; runs need contacts and deals read and write, companies
read, and schema read.

A lane pushing to `hubspot:contacts` finds or creates the person's contact;
one pushing to `hubspot:deals` also opens one deal per company (named by its
domain) or adds the contact to the company's open deal. Before every push,
leadscore reads HubSpot for the leads it may push: a contact that opted out
of email makes the lead `unsubscribed`, and a company with an open or won deal
keeps its people out of cold lanes until the deal is closed lost.

### The receiver

`leadscore serve` accepts Apollo workflow requests at `POST /apollo/reply`
(sent, replies, opt-outs) and `POST /apollo/visit` (website visits), with
bodies from the templates in `setup/apollo/`. Each request carries the
receiver secret in the `X-Leadscore-Secret` header (or, when Apollo cannot
set headers, in a top-level `leadscore_secret` body field, which is removed
before storing; prefer the header, since a flood of slow senders can delay
body-secret requests but never header ones). The receiver answers 200 only
once the event is stored; a wrong or missing secret gets 401, and a request
it could not store within 10 seconds gets 503 so Apollo can send it again. A
repeated event is counted once.

An opt-out or reply the receiver stores while a run is pushing still counts:
before each batch of 25 pushes the run reads the newly stored events, so the
person is not pushed in the next batch. Every opt-out and reply is kept in
`Outcomes`, so it holds long after the 90-day event window.

**Silence.** Reachable is not delivering. When the receiver is set up
(`receiver.public_url` is set), each run checks that every event kind it
expects arrived within `silence_threshold` (3 days by default): `sent` with
`replies: receiver`, and each `receiver.visit_events` kind. A silent kind
shows in `Health` as `silent:<kind>` and makes the run unhealthy; check that
Apollo workflow. Without `receiver.public_url`, silence is not watched, and
`doctor` says so (`receivers:no_public_url`); `doctor` also calls
`<public_url>/healthz` to check Apollo can reach the receiver.

**Receiver-only leads.** A lead known only from webhooks could be forged by
anyone with the secret. When a lane pushes one, `Health` shows the warning
`receiver_only_push:<lead>` until a source (a CSV or Sheet tab) reports that
person too. Cold lanes should require `receiver_only` to be false.

`/healthz` with the timer is 200 while the last run succeeded or none is due
yet, and 503 when the last run failed or none succeeded in three intervals;
without the timer (Google Cloud) it is 200 unless storing events fails.

#### Keep the receiver secret private

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
secret" under Path 2.

## For developers

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
| `internal/store/codec` | maps the model to table writes; loads a store and checks its schema version |
| `internal/store/sqlite` | the built-in SQLite store (WAL, lease row, event log) |
| `internal/store/sheets` | the built-in Google Sheets store (one batchUpdate per commit, monthly `Events` tabs, Cloud Storage lease file) and `setup sheet` |
| `internal/fakes/sheets`, `internal/fakes/gcs` | in-memory fakes of Google Sheets, Drive and Cloud Storage for tests |
| `internal/rules` | the rubric compiler and evaluator: YAML rules compiled to CEL |
| `internal/merge` | turns input rows into one lead per person: header aliases, identities, `same_as` merges, the Overrides tab |
| `internal/receiver` | `leadscore serve`: the Apollo webhook receiver, its write queue, `/healthz` and the Docker run timer |
| `internal/receiver/auth` | the constant-time secret check |
| `internal/engine` | the run: lease, sources and chunked merge, scoring, the two saves, `Ranked`, `Health`, lanes and the ledger, export lists and their CSVs, the Sheet view; later steps plug in as hooks |
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
| `docs/design/` | the design notes behind it (not shipped in releases) |

### Build and test

```sh
go build ./...
go vet ./...
go test -race ./...
```

`LEADSCORE_LIVE_SHEETS=1` (or the path of a service-account key file) also
runs the live Sheets check: it saves 20,000 leads and a year of events to a
scratch spreadsheet and loads them within a minute. Service accounts have no
Drive storage, so, as in `setup sheet`, your own gcloud login (or the account
`LEADSCORE_LIVE_SHEETS_OWNER` names) creates the spreadsheet; it needs
`gcloud auth login --enable-gdrive-access`. Without it the check is skipped.

`LEADSCORE_LIVE_CLOUDRUN=1`, with `LEADSCORE_LIVE_PROJECT` (a billed,
throwaway project, never `leadscore-dev`) and `LEADSCORE_LIVE_IMAGE`, runs
the live Google Cloud check: it sets the project up with `setup/gcp.sh`,
starts a run longer than 3 minutes from Cloud Scheduler, posts webhook bursts
during the run and during a redeploy, and checks no event was lost. Its
comment lists what to do first. Without it the check is skipped.
`setup/gcp.sh` itself is tested offline against a fake `gcloud`
(`testdata/gcloud/gcloud`).
