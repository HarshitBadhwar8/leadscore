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
| `internal/check` | the `doctor` check framework and the `secrets` and `rubric` checks |
| `internal/rules` | the rubric compiler and evaluator: YAML rules compiled to CEL |
| `internal/logredact` | log redaction: logs carry ids, never emails |
| `storetest/`, `sinktest/` | conformance suites for plug-in stores and sinks |
| `examples/` | a made-up example rubric (`rubric.yml`) and a sample lead sheet (`leads.csv`) |

## Build and test

```sh
go build ./...
go vet ./...
go test -race ./...
```

## Commands

`leadscore help` lists every command. Only `config get`, `config set-hosting` and
`rules check` work so far; every other command prints `not built yet (slice S<n>)`
and exits 2.

Without `--config`, commands read `/config/bundle.yaml`, else
`/config/leadscore.yml`, else `./leadscore.yml`.

## The rubric

Your ideal customer is one YAML file, the rubric: which columns matter, how to
label each company and person (tier, priority, ...), how to score them, and
where each lead goes. Start from `examples/rubric.yml`; the full format is
section 2 of `docs/design/oss-outbound-engine-contracts.md`. Check a rubric
with `leadscore rules check rubric.yml`, which lists every problem with its line.
