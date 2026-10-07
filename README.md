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
| `internal/check` | the `doctor` check framework and the `secrets`, `store`, `rubric`, `overrides` and `duplicates` checks |
| `internal/model` | the in-memory model of the store's tables |
| `internal/store/codec` | maps the model to table writes; loads a store and checks its schema version |
| `internal/store/sqlite` | the built-in SQLite store (WAL, lease row, event log) |
| `internal/store/sheets` | the built-in Google Sheets store (one batchUpdate per commit, monthly `Events` tabs, Cloud Storage lease file) and `setup sheet` |
| `internal/fakes/sheets`, `internal/fakes/gcs` | in-memory fakes of Google Sheets, Drive and Cloud Storage for tests |
| `internal/rules` | the rubric compiler and evaluator: YAML rules compiled to CEL |
| `internal/merge` | turns input rows into one lead per person: header aliases, identities, `same_as` merges, the Overrides tab |
| `internal/engine` | the run: lease, sources and chunked merge, scoring, the two saves, `Ranked`, `Health`, lanes and the ledger, export lists and their CSVs; later steps plug in as hooks |
| `internal/logredact` | log redaction: logs carry ids, never emails |
| `adapters/csv` | the CSV file source (`type: csv`): lead rows, or event rows with `events: true` |
| `adapters/sheetsource` | the Google Sheet tab source (`type: sheetsource`): tabs of the team's spreadsheet |
| `storetest/`, `sinktest/` | conformance suites for plug-in stores and sinks |
| `examples/` | a made-up example rubric (`rubric.yml`) and a sample lead sheet (`leads.csv`) |

## Build and test

```sh
go build ./...
go vet ./...
go test -race ./...
```

`LEADSCORE_LIVE_SHEETS=1` (or the path of a service-account key file) also runs the
live Sheets check: it saves 20,000 leads and a year of events to a scratch
spreadsheet and loads them within a minute. Without it the check is skipped.

## Commands

`leadscore help` lists every command. So far `run`, `status`, `ranked`,
`explain`, `config get`, `config set-hosting`, `rules check`, `setup sheet` and
the Overrides writers work; every other command prints `not built yet (slice S<n>)` and
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
person who clicked an unsubscribe link without replying is not seen before a
push, unless the receiver takes Apollo's reply workflow (whose `unsubscribed`
event reports it) or HubSpot is also a sink. The `apollo-key` check warns
(`apollo-key:no_optout_flag`) in that case.

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

## The rubric

Your ideal customer is one YAML file, the rubric: which columns matter, how to
label each company and person (tier, priority, ...), how to score them, and
where each lead goes. Start from `examples/rubric.yml`; the full format is
section 2 of `docs/design/oss-outbound-engine-contracts.md`. Check a rubric
with `leadscore rules check rubric.yml`, which lists every problem with its line.
