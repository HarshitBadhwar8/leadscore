# Setting up leadscore

This is the step-by-step runbook for the three ways to run leadscore. The
README's [Choosing a path](../README.md#choosing-a-path) helps you pick one.
Follow one path from top to bottom.

A coding agent can follow a path too, since each step says who does it:

- **[agent]**: the agent (or you) runs it.
- **[person]**: a person must do it: paste a key, approve billing, create an
  account, or say yes to a dry run. An agent stops and asks.

On every path, runs start with **pushes off**. leadscore scores and lists,
a person reviews a dry run, and only then turns pushing on.

**There is no alerting in v1, on any path.** Nobody is emailed or messaged
when a run fails. Someone has to look (`leadscore status`, the `Health` tab,
`docker ps`), or point an uptime monitor at the receiver's `/healthz`.

## Before you start

**[person]** Have ready, for the path you picked:

- **Every path:** a terminal, and your leads as a CSV file (or a Google
  Sheet tab) with a header row. Columns are matched by name. `Email`,
  `Full Name`, `Job Title`, `Company`, `Website`, `LinkedIn` and their usual
  spellings work as they are.
- **The Apollo sink or Apollo enrichment:** a paid Apollo plan (workflows
  and the API need it) and an Apollo master API key.
- **The HubSpot sink:** a HubSpot private app token.
- **CSV only:** neither. No vendor account is needed.
- **Docker:** Docker (Docker Desktop or Colima on macOS, Docker Engine on
  Linux) or podman with compose.
- **Docker on a server:** a VM with a public IP, ports 80 and 443 open, and a
  domain (or subdomain) whose DNS points at it.
- **Google Cloud:** a Google account that may create a project with billing,
  and `gcloud` installed and logged in (`gcloud auth login`).

## Install the CLI

The Docker path needs no CLI on your machine, because every command runs
inside the container. You need it for **CSV only as a plain binary**, for
**Google Cloud**, and to make a Google Sheet (`setup sheet`) on any path.

- **[agent]** From a release: download the archive for your OS from the
  [Releases page](https://github.com/HarshitBadhwar8/leadscore/releases).
  Each archive holds the `leadscore` binary, `LICENSE`, `NOTICE` and the
  third-party licenses. Put the binary on your `PATH` as `leadscore`.
- **[agent]** From source, with Go 1.25 or newer:

  ```sh
  git clone https://github.com/HarshitBadhwar8/leadscore.git
  cd leadscore && go build -o ~/.local/bin/leadscore ./cmd/leadscore
  leadscore version   # check: ~/.local/bin (or the folder you chose) must be on your PATH
  ```

The container image also works for most commands. It does not work for
`setup sheet` (it signs in with your machine's `gcloud`) or `setup/gcp.sh`
(it calls `leadscore` on your `PATH`):

```sh
docker run --rm -v ~/.config/gcloud:/home/leadscore/.config/gcloud:ro -v "$PWD":/config \
  ghcr.io/harshitbadhwar8/leadscore:v0.1.0-rc.1 doctor
```

Every command takes `--config <file>` and `--rubric <file>`, and
`leadscore <command> --help` lists a command's flags. Without `--config`,
commands look for their settings in this order:

1. `/config/bundle.yaml`, the file on Google Cloud that holds both
   `leadscore.yml` and the rubric (see Path 2, step 8).
2. `/config/leadscore.yml`, the Docker mount.
3. `./leadscore.yml`, so run commands from the folder holding it.

## Path 1: CSV only

CSV files in, scored lists out. No vendor accounts.

1. **[agent]** Make a folder for the install and copy in, from this
   repository's `examples/`: `leadscore.csv-only.yml` as `leadscore.yml`,
   `rubric.csv-only.yml` as `rubric.yml`, and your CSV as `leads.csv` (or
   `examples/leads.csv` to try it with made-up leads).
2. **[person]** Write the rubric for your ideal customer. Start from the
   example, and see [The rubric](../README.md#the-rubric). Check it with
   `leadscore rules check rubric.yml`. The example rubric has export lanes
   only, on purpose. A cold lane claims the leads it matches even before
   Apollo or HubSpot is set up, and they show `do_not_contact: yes` on every
   list.
3. Run it, as a plain binary or on Docker:
   - **Plain binary. [agent]** In `leadscore.yml`, change `store.path` to
     `leadscore.db` and `export.dir` to `out` (paths are relative to the
     folder). Set a receiver secret in this terminal only, because doctor
     checks it is set even though a CSV-only install receives no webhooks:
     `export LEADSCORE_RECEIVER_SECRET=$(openssl rand -hex 32)`. A new
     terminal needs the line again, which is fine, since nothing uses the
     value. Then run `leadscore run --dry-run` to see what a run would do,
     writing nothing. It ends with a summary line starting `dry run` (it says nothing was saved or pushed). On a first
     install the lead ids it prints are temporary, because no run has saved
     the leads yet. Then run `leadscore run`. From then on, look a lead up by
     email with `leadscore explain <email>`, or take its id from
     `leadscore ranked`.
   - **Docker. [agent]** Follow [Path 3](#path-3-docker-a-laptop-or-a-server)
     with these files, and skip its Apollo and HubSpot steps.
4. **[agent]** Run `leadscore doctor` until it prints `0 failed` (on Docker,
   `docker compose exec leadscore leadscore doctor`). With CSV only, expect
   two warnings: `pushes-enabled:off` and `receivers:no_public_url`.
5. **[agent]** See the results: `leadscore ranked` (every lead, best first),
   `leadscore explain <email>` (why a lead scored as it did), and one CSV per
   export lane in `out/` (`out/call-list.csv`, `out/nurture.csv` with the
   example rubric). **Filter on `do_not_contact` before every send.** Drop
   every row where it is `yes`.

Run `leadscore run` again whenever your CSV changes, or let Docker run it
every 15 minutes. A lead is added to a list once, and each run refreshes its
`status` and `do_not_contact`.

## Path 2: Google Cloud

The receiver runs as a Cloud Run service, and each run as a Cloud Run job
that Cloud Scheduler starts. The store is a Google Sheet you own. Run every
step from one folder on your machine (macOS, Linux or Google Cloud Shell),
with the CLI installed.

`setup/gcp.sh` is in this repository. It changes `leadscore.yml` only through
`leadscore config set-hosting`. Add `--dry-run` to any `setup/gcp.sh` step to
print what it would change without changing it.

1. **[person]** Prerequisites: `gcloud` installed and logged in as someone
   who may create projects, service accounts, Cloud Run services and jobs,
   Cloud Scheduler jobs and Artifact Registry repositories, and grant roles
   on them. The vendor accounts depend on your sinks (see
   [Before you start](#before-you-start)). With export lanes only, you need
   none.
2. **[agent]** Install the CLI (above). Copy `examples/leadscore.gcp.yml` as
   `leadscore.yml` and `examples/rubric.yml` as `rubric.yml` into an empty
   folder, and `setup/gcp.sh` beside them (or call it by its path).
3. **[person]** Create or pick a Google Cloud project, and turn billing on
   for it. Google asks a person to approve billing.
4. **[agent]** `setup/gcp.sh accounts --project <id> [--region asia-south1]`
   enables the APIs (Cloud Run, Cloud Scheduler, Secret Manager, Cloud
   Storage, Sheets, Drive, Artifact Registry, IAM Service Account
   Credentials). It creates the run account (`leadscore-run`) and the
   receiver account (`leadscore-receiver`), and writes the `hosting` block.
   It ends by signing your local commands in as the run account.
   **[person]** A browser opens: sign in.
   - **From then on, every program on your machine that uses Google's
     application-default login acts as the run account.** To undo it, run
     `gcloud auth application-default revoke`.
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
   - **The spreadsheet holds personal data (names, emails).** Share it only
     with named people, never by link.
7. **[agent]** `setup/gcp.sh secrets` creates the secrets. **[person]** It
   asks for each API key at a hidden prompt. Paste it, or press Enter to skip
   a key you do not use. It generates the receiver secret and never prints it
   (see [Keep the receiver secret private](../README.md#keep-the-receiver-secret-private)).
8. **[person]** Write the rubric (see [The rubric](../README.md#the-rubric)),
   and put your leads in the spreadsheet's `Leads` tab. **[agent]** If you
   use HubSpot, add the `sinks.hubspot` block and run
   `leadscore setup hubspot`. Then run `leadscore config push`, which uploads
   `leadscore.yml` and the rubric together as one version of the
   `leadscore-config` secret. Cloud Run reads that version as
   `/config/bundle.yaml`. It refuses:
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
   seconds, with no retries. The step prints the receiver's address.
   **[agent]** Set `receiver.public_url` to it and run
   `leadscore config push` again. For `<image>`:
   - **A release image,** such as
     `ghcr.io/harshitbadhwar8/leadscore:v0.1.0-rc.1`. Cloud Run cannot pull
     from ghcr.io, so for any `ghcr.io/` image this step creates an Artifact
     Registry remote repository, `ghcr-proxy`, that fetches it from ghcr.io.
     It lets the run account read that repository, and deploys the image
     through it.
   - **Before a release image exists,** build the image yourself and push it
     to an Artifact Registry repository in the same project. The step deploys
     any image outside ghcr.io as it is. Replace `P` with your project id and
     `R` with your region:

     ```sh
     gcloud artifacts repositories create leadscore --repository-format docker --location R --project P
     gcloud auth configure-docker R-docker.pkg.dev
     docker buildx build --platform linux/amd64 -t R-docker.pkg.dev/P/leadscore/leadscore:dev --push .
     setup/gcp.sh deploy R-docker.pkg.dev/P/leadscore/leadscore:dev
     ```
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
    It names leads by id and ends with a summary line starting `dry run` (it says nothing was saved or pushed). Look a lead
    up by email with `leadscore explain <email>`, or take its id from
    `leadscore ranked`. **[person]** Approve it, or change the rubric and
    repeat.
14. **[agent]** Only after that approval: set `pushes_enabled: true` and run
    `leadscore config push`. The next run pushes.
15. **[person]** Nothing alerts you when a run fails. Set an uptime monitor
    on `<public_url>/healthz`, and look at the `Health` tab now and then.

**Changing settings or rules later** means editing the files and running
`leadscore config push`. The next run reads them, with no redeploy, and
`SKILL.md` has the full loop. A new `schedule` also needs
`setup/gcp.sh schedule`, and a new `deadline` needs `setup/gcp.sh redeploy`.
Each run records the bundle version it read in `State` (`config_version`).
`doctor`'s `rubric-version` line warns when the last run used another rubric
than your file.

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

**Rotating a secret. [person]** Command 3 in the block below prints the new
secret, so a person runs this block, not an agent. On macOS, add `| pbcopy`
to the end of command 3 so the secret never shows. No webhook is refused at
any point. Replace `P` with your project id:

```sh
# 1. Keep the current secret as the previous one, and add a new current one.
gcloud secrets versions access latest --secret receiver-secret --project P |
  gcloud secrets versions add receiver-secret-previous --project P --data-file=-
openssl rand -hex 32 | tr -d '\n' |
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

## Path 3: Docker (a laptop or a server)

One container runs the receiver and a run every `schedule`. The store is
SQLite on a named volume.

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
  in HubSpot (see [Opt-outs](../README.md#opt-outs) for Apollo).

Steps:

1. **[agent]** Make a folder and copy in `compose.yaml` from this repository,
   the example `leadscore.yml` for your setup, the example rubric as
   `rubric.yml`, and your CSV files. The example `leadscore.yml` is
   `examples/leadscore.laptop.yml` or `examples/leadscore.server.yml`, renamed
   `leadscore.yml` (or `examples/leadscore.csv-only.yml` for CSV only). The
   folder is mounted read-only at `/config`, and the export lists land in
   `./out`.
   - The export lists hold personal data, so make the `out` folder private:
     `mkdir -p out && chmod 700 out`.
   - On macOS with Colima, keep the folder under your home folder. Colima
     shares only that with Docker, and a folder elsewhere shows up empty
     inside the container.
   - On Linux the container runs as its own user (uid 10001, the
     Dockerfile's `USER`). Run `sudo chown 10001:10001 out`, so the container
     can write the lists into `./out`. Then run
     `chmod o+rx . && chmod o+r leadscore.yml rubric.yml *.csv`, so the
     container can read your settings, rubric and leads (not secrets).
2. **[person]** Create `.env` in the folder, holding the keys (only those you
   use) and a receiver secret:

   ```sh
   APOLLO_API_KEY=...
   HUBSPOT_TOKEN=...
   LEADSCORE_RECEIVER_SECRET=...   # the output of: openssl rand -hex 32
   LEADSCORE_PORT=8080             # optional: the receiver's port on this machine (127.0.0.1)
   ```

   Keep `.env` private with `chmod 600 .env`. Docker reads it, the container
   does not. **Never commit `.env` (or a service-account key file) to git.**

   `compose.yaml` runs the release image
   `ghcr.io/harshitbadhwar8/leadscore:v0.1.0-rc.1`. Before a release image
   exists, build the image yourself from this repository with
   `docker build -t leadscore .`, and add `LEADSCORE_IMAGE=leadscore` to
   `.env`.
3. **[person]** Write the rubric from your CSV headers and the example (see
   [The rubric](../README.md#the-rubric)). **[agent]** Check it with
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
   and show the result to a person. It names leads by id and ends with a
   summary line starting `dry run` (nothing was saved or pushed). Look a lead up by email with
   `docker compose exec leadscore leadscore explain <email>`, or take its id
   from `ranked`. **[person]** Approve it, or change the rubric and repeat.
9. **[agent]** Only after that approval: set `pushes_enabled: true` in
   `leadscore.yml`. The next run picks it up, with no restart.
10. **[person]** Nothing alerts you when a run fails. On a server or a
    tunnel, set an uptime monitor on `<public_url>/healthz`. On a polling
    laptop, check `docker ps` now and then.

Every command runs inside the container:
`docker compose exec leadscore leadscore <command>`. `docker ps` shows the
container unhealthy when `/healthz` does: the last run failed, none
succeeded in three `schedule` intervals, or the store cannot be read. Each
run logs one summary line (`docker compose logs leadscore`). The compose file
assumes the receiver's port 8080 inside the container, so leave
`receiver.port` unset.

**Upgrading** is changing the image tag in `compose.yaml` (or
`LEADSCORE_IMAGE`, which overrides it), `docker compose up -d`, then
`doctor`. The receiver is down for those few seconds, so webhooks sent then
rely on Apollo retrying. Rolling back is the previous tag. Copy the SQLite
volume first if you want a backup for a store damaged some other way.

### A Google Sheet on Docker

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
  run `setup/gcp.sh`: it is the Google Cloud path's script. The same rule
  holds: share the spreadsheet only with named people, never by link.
