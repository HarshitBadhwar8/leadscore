---
name: leadscore
description: Run and maintain a leadscore install, the open-source outbound engine that merges leads, scores them with a YAML rubric, routes each to one lane, and pushes to Apollo, HubSpot or export lists. Use when setting leadscore up, changing its rubric, reading `leadscore doctor` or `leadscore status`, or fixing a failed run.
---

# leadscore

leadscore is one binary. `leadscore serve` receives Apollo webhooks (and, on
Docker, runs the loop on a timer); `leadscore run` is one run of the loop. The
team's rules live in two files: `leadscore.yml` (settings) and the rubric
(`rubric.yml`); `docs/reference.md` has the full format of both. Setting it up is the
README, step by step; this file is for running it afterwards.

Where commands run:

- **Docker:** `docker compose exec leadscore leadscore <command>`, from the
  folder holding `compose.yaml`.
- **Google Cloud:** `leadscore <command>` on your own machine, in the folder
  holding `leadscore.yml` and the rubric (after `setup/gcp.sh accounts`).
- **Plain binary:** `leadscore <command> --config <path to leadscore.yml>`.

Read before you act: `leadscore doctor` (one line per check, each problem
with its fix), `leadscore status` (the last run and every open problem),
`leadscore ranked`, `leadscore explain <person>` and `leadscore facts` (each
company's stored facts and where they came from). None of them writes
anything.

## Rules you must keep

- Never turn on `pushes_enabled` before a person has reviewed
  `leadscore run --dry-run` and said yes.
- Never paste an API key or the receiver secret into chat, an issue,
  `leadscore.yml` or the rubric. Keys go in `.env` (Docker) or Secret Manager
  (Google Cloud), typed by a person.
- Never edit the tool tabs or tables (`People`, `Pushes`, `Outcomes`,
  `Health`, `State`, ...) by hand to make a problem go away. Change a lead
  through `Overrides`: `leadscore set-status`, `merge`, `mark-distinct`,
  `retry`. The one exception is a `merge_cycle` (see "duplicates" below),
  which is fixed in `People`.
- Before sending from an export list, filter on `do_not_contact`.

## Changing the rules (the rule-change loop)

1. Edit the rubric (or `leadscore.yml`).
2. `leadscore rules check rubric.yml` until it prints no errors.
3. `leadscore run --dry-run`: one line per lead whose verdict, status or
   planned lane would change, then totals. It writes and spends nothing.
4. Show a person the changes and get a yes. If a change looks wrong,
   `leadscore explain <person>` shows why a lead scored as it did; go back to
   step 1.
5. Make it live:
   - Docker or plain binary: nothing to do; the next run reads the files.
   - Google Cloud: `leadscore config push` (uploads both files as one
     version; the next run uses them, with no redeploy).
6. After the next run, `leadscore doctor`. The `rubric-version` line is ok
   when the run scored with the file you edited.

Changing `schedule` on Google Cloud also needs `setup/gcp.sh schedule`, and a
new `deadline` needs `setup/gcp.sh redeploy`.

## Troubleshooting

Each section is one `leadscore doctor` check (`docs/reference.md`, "Doctor checks"). The
line doctor prints is `FAIL  <check>: <key>: <message>` (or `warn` for a
warning), with `fix:` under it. Checks marked "in run" also run inside every
run, so the same problem shows in `leadscore status` under its key.

### secrets

In run. `secret_missing:<VARIABLE>`: an adapter in `leadscore.yml` needs a
key that is not set (`APOLLO_API_KEY` for `enrich` or `sinks.apollo`,
`HUBSPOT_TOKEN` for `sinks.hubspot`). Docker: a person adds it to `.env`, then
`docker compose up -d`. Google Cloud: a person runs `setup/gcp.sh secrets`;
if it says the run account cannot read the secret, run
`setup/gcp.sh accounts` again.

### receiver-secret

`serve` start and doctor. `secret_missing:LEADSCORE_RECEIVER_SECRET`: the
install expects webhooks (`replies: receiver`, the default, or
`receiver.visit_events`) and has no receiver secret, so every webhook is
refused. A person generates one (`openssl rand -hex 32`), puts it in `.env`
(Docker), Secret Manager (Google Cloud) or the shell profile (plain binary),
and in each Apollo workflow. A CSV-only install still needs
one set while `replies` is `receiver`. `secret_previous:...` (a warning): a
rotation is not finished; once every workflow sends the new secret, remove
`LEADSCORE_RECEIVER_SECRET_PREVIOUS`.

### hosting

Doctor only, Google Cloud only. A `hosting:<what>` line names the piece
`setup/gcp.sh` made that is missing or wrong (service, job, scheduler,
`ghcr-proxy`, an account, a timeout, the config version). Run the step the
fix names (`setup/gcp.sh deploy <image>`, `redeploy`, `schedule`) or
`leadscore config push`. `hosting:config` means `hosting.project` or
`hosting.region` is missing: run `setup/gcp.sh accounts`.

### rubric-version

Doctor only, a warning. `rubric-version:differs`: the last run scored with
another rubric than the local file. Docker: the next run picks the file up;
review `leadscore run --dry-run` first. Google Cloud: `leadscore config push`
the reviewed file, or put back the file the run used.

### sheet-access

In run. `sheet-access:<spreadsheet id>`: this account cannot open the
spreadsheet (the store, or the SQLite view). `sheet-access:<account>`: on
Google Cloud, the run or receiver account is not an editor. Run
`leadscore setup sheet --repair` (it shares the sheet again). If Drive says a
Workspace sharing policy blocks it, a person asks the Workspace admin to allow
sharing that file with the service accounts (`gserviceaccount.com`).

### sheets

In run. `sheets:recalc` or `sheets:timezone`: the spreadsheet's settings
moved, so the `Health!H1` staleness formula is wrong. Run
`leadscore setup sheet --repair`. `sheets:cells` (a warning): the spreadsheet
uses over 70% of Google's 10 million cells; shorten `log_retention` or move to
SQLite.

### hubspot

In run, only with `sinks.hubspot` and `HUBSPOT_TOKEN`. `hubspot:scopes`:
re-create the private app with the scopes the README lists.
`hubspot:properties`: run `leadscore setup hubspot`. `hubspot:pipeline`: set
`sinks.hubspot.pipeline` and `stage` to names HubSpot has (an open stage).
`hubspot:unknown_stage`: a deal step waits on a stage no pipeline lists; check
the deal in HubSpot. `hubspot:config`: fix the block (quote text values).
`hubspot:api`: HubSpot did not answer; check the token and HubSpot's status.

### apollo-key

In run. `apollo-key:auth`: Apollo refuses the key; a person checks
`APOLLO_API_KEY`. `apollo-key:unreachable` (a warning): Apollo did not
answer; nothing to do if it clears. `apollo-key:config`: the `enrich` or
`sinks.apollo` block cannot be used; fix it (`base_url` is for tests only).
`apollo-key:no_optout_flag` (a warning): sending only through Apollo, and no
unsubscribe webhook has arrived yet; add the unsubscribe workflow from
`setup/apollo/`, or mark such people `unsubscribed` with
`leadscore set-status`.

### apollo-sequences

In run. `apollo-sequences:mailbox`: `sinks.apollo.mailbox_id` (quoted; the
mailbox's id or its email address) is not one of the account's mailboxes. `apollo-sequences:<lane>`: the lane's
sequence name is missing or not unique in Apollo; names must match exactly.
`apollo-sequences:key`: the key is not a master key; a person makes one.
`apollo-sequences:unreachable` (a warning): Apollo did not answer.
`apollo-sequences:mailbox_address` (a warning, doctor only): `mailbox_id` is an
address; it names the mailbox id it resolves to. Nothing to fix.

### receivers

Doctor only. `receivers:unreachable`: `receiver.public_url` + `/healthz`
does not answer as leadscore. Google Cloud: `setup/gcp.sh deploy`; a server:
check DNS, ports 80 and 443, and `docker compose --profile caddy up -d`; a
laptop: use `replies: polling` or a Cloudflare Tunnel. `receivers:unhealthy`
(a warning): it answers but the runs are unhealthy; read
`leadscore status`. `receivers:no_public_url` (a warning): no address is set,
so doctor cannot probe it and `receiver-silence` is off; set it once the
receiver is public (a CSV-only install can leave it).

### receiver-silence

In run, only when `receiver.public_url` is set. `silent:<kind>`: no event of
that kind (`sent`, or a `visit_<name>`) arrived within `silence_threshold`.
Open the Apollo workflow that sends it and check its URL, secret and trigger;
then check `receivers` passes.

### lease

Doctor only. `lease:bucket`: on Sheets, the lease bucket is missing or this
account cannot write it, so no run can start. Run `setup/gcp.sh bucket`.
`lease:held` (a warning): a run holds the lease now, or one stopped without
releasing it; it clears itself at the expiry shown. Never delete the lease by
hand while a run may hold it.

### pushes

In run. `push_failed:<lead>:<lane>:<step>`: a push failed three times. Read
the row's `last_error` in `Pushes`, fix the cause, then
`leadscore retry --lane <lane> <person>` (or `leadscore retry` for every
failed push). `push_pending` (a warning): steps waiting over 24 hours; check
`leadscore status` for what blocks pushing (pushes off, a backlog, a lookup).

### store

In run. `store:newer_schema`: a newer version wrote the store; install a
matching version. `store:cloud_run_files`: SQLite or a CSV path on Cloud Run,
which keeps no files; use the Sheets store and Sheet-tab sources.
`ledger_shrank`: `Pushes` lost rows, so pushing is blocked; restore them from
a copy. `store:disk_not_kept`: the SQLite file is not on a named volume.
`store:opened_outside_container`: run the command inside the container
(`docker compose exec`). `store:not_created` (a warning, doctor only): no run
has saved yet; wait for the first run.

### rubric

In run. `rubric_invalid:compile`: run `leadscore rules check rubric.yml` and
fix each line it names. `rubric_unknown_field:<field>`: the rubric reads a
column no source has; fix the name, add the column, or declare it under
`fields`.

### overrides

In run. `status_conflict:<lead>`: two status rows for one person disagree, so
the lead is blocked; keep one with `leadscore set-status`.
`override_unmatched:<row>`: the row names nobody known yet (a warning; it
waits), or an invalid value (fix the cell). `override_unknown_lane:<row>` (a
warning): a `retry` row names a lane the rubric lacks.

### pushes-enabled

Doctor only, a warning. `pushes-enabled:off`: runs score and keep export
lists but push nothing. After a person approves a dry run, set
`pushes_enabled: true` (Google Cloud: then `leadscore config push`).
`cold_lane_no_sink:<lane>` (a warning): a cold lane pushes to a sink with no
`sinks.<type>` block, yet it already claims the leads it matches, so they are
`do_not_contact` on every export list. Set the sink up, or remove the lane (a
CSV-only team uses export lanes only).

### duplicates

In run. `namesake:<lead>`: two leads share a company and a name; decide with
`leadscore merge <a> <b>` (same person) or `leadscore mark-distinct <a> <b>`.
`merge_cycle:<lead>`: `merged_into` was hand-edited into a loop; a person
clears the wrong `merged_into` cell in `People` (the one exception to never
editing tool tabs). `key_conflicts` (a warning): rows carried an email or LinkedIn URL
another lead holds; check the sources.
