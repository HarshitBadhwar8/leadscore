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
| `internal/check` | the `doctor` check framework and the `secrets`, `store` and `rubric` checks |
| `internal/model` | the in-memory model of the store's tables |
| `internal/store/codec` | maps the model to table writes; loads a store and checks its schema version |
| `internal/store/sqlite` | the built-in SQLite store (WAL, lease row, event log) |
| `internal/store/sheets` | the built-in Google Sheets store (one batchUpdate per commit, monthly `Events` tabs, Cloud Storage lease file) and `setup sheet` |
| `internal/fakes/sheets`, `internal/fakes/gcs` | in-memory fakes of Google Sheets, Drive and Cloud Storage for tests |
| `internal/rules` | the rubric compiler and evaluator: YAML rules compiled to CEL |
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

`leadscore help` lists every command. Only `config get`, `config set-hosting`,
`rules check` and `setup sheet` work so far; every other command prints
`not built yet (slice S<n>)` and exits 2.

Without `--config`, commands read `/config/bundle.yaml`, else
`/config/leadscore.yml`, else `./leadscore.yml`.

`leadscore setup sheet` creates the Sheets store's spreadsheet with your own Google
login (`gcloud auth login --enable-gdrive-access` first), shares it with the run and
receiver accounts, and writes `store.spreadsheet`; `--view` makes a SQLite store's
read-only view; `--repair` puts an existing spreadsheet's settings back.

## The rubric

Your ideal customer is one YAML file, the rubric: which columns matter, how to
label each company and person (tier, priority, ...), how to score them, and
where each lead goes. Start from `examples/rubric.yml`; the full format is
section 2 of `docs/design/oss-outbound-engine-contracts.md`. Check a rubric
with `leadscore rules check rubric.yml`, which lists every problem with its line.
