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
| `internal/engine` | the run: lease, sources and chunked merge, scoring, the two saves, `Ranked`, `Health`; later steps plug in as hooks |
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
  or finished unhealthy. Enrichment, events and pushing arrive with later
  slices; until then a lane whose sink is not in the build is reported in
  `Health`.
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

Without `--config`, commands read `/config/bundle.yaml`, else
`/config/leadscore.yml`, else `./leadscore.yml`.

## The rubric

Your ideal customer is one YAML file, the rubric: which columns matter, how to
label each company and person (tier, priority, ...), how to score them, and
where each lead goes. Start from `examples/rubric.yml`; the full format is
section 2 of `docs/design/oss-outbound-engine-contracts.md`. Check a rubric
with `leadscore rules check rubric.yml`, which lists every problem with its line.
