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
| `internal/check` | the `doctor` check framework and the `secrets` and `store` checks |
| `internal/model` | the in-memory model of the store's tables |
| `internal/store/codec` | maps the model to table writes; loads a store and checks its schema version |
| `internal/store/sqlite` | the built-in SQLite store (WAL, lease row, event log) |
| `internal/logredact` | log redaction: logs carry ids, never emails |
| `storetest/`, `sinktest/` | conformance suites for plug-in stores and sinks |

## Build and test

```sh
go build ./...
go vet ./...
go test -race ./...
```

## Commands

`leadscore help` lists every command. Only `config get` and `config set-hosting`
work so far; every other command prints `not built yet (slice S<n>)` and exits 2.
Without `--config`, commands read `/config/bundle.yaml`, else
`/config/leadscore.yml`, else `./leadscore.yml`.
