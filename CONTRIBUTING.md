# Contributing

Bug reports, fixes and new adapters are welcome. Open an issue first for anything larger than
a fix, so the change can be agreed before you write it. Report a security problem privately,
through this repository's Security tab, not as an issue.

## Build and test

You need Go 1.25 or later. CI tests Go 1.25, 1.26 and 1.27.

```sh
go build ./... && go vet ./... && go test -race ./...
```

The tests need no account and no key. Vendors and Google Cloud are faked in `internal/fakes`.
CI also builds and vets for Windows:

```sh
GOOS=windows go build ./... && GOOS=windows go vet ./...
```

## Live checks

A few tests call real services. Each skips unless its variable is set, and none runs in CI.

| Variable | What it runs |
|---|---|
| `LEADSCORE_LIVE_APOLLO` | Read-only Apollo calls. Set it to an env file holding `APOLLO_API_KEY` and `APOLLO_MAILBOX_ID`. |
| `LEADSCORE_LIVE_SHEETS` | The Sheets store against real Google Sheets: 20,000 leads and a year of events in a scratch spreadsheet. Set it to `1`, or to a service-account key file. `LEADSCORE_LIVE_SHEETS_OWNER` names the gcloud login that creates the spreadsheet. |
| `LEADSCORE_LIVE_CLOUDRUN` | The whole Google Cloud setup in a throwaway project. It also needs `LEADSCORE_LIVE_PROJECT`, `LEADSCORE_LIVE_IMAGE` and `LEADSCORE_LIVE_DENY_PROJECTS`, the projects it must never touch. The comment on `TestLiveCloudRun` in `cloudrun_live_test.go` lists the steps before it. |

Use a throwaway account or project, never one a real install uses.

## Checks CI runs

| Check | Command |
|---|---|
| Format | `gofmt -l .` prints nothing |
| Lint | `golangci-lint run` with golangci-lint v2.14.0 |
| Shell | `shellcheck setup/gcp.sh scripts/*.sh testdata/gcloud/gcloud testdata/compose/smoke.sh` with shellcheck 0.11.0 |
| Dockerfile | `hadolint --ignore DL3018 Dockerfile` |
| Vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` on Go 1.27.x |
| Third-party licenses | `go install github.com/google/go-licenses/v2@v2.0.1`, then `scripts/third-party-licenses.sh --check` |
| Docker | `docker build -t leadscore:ci .`, then `testdata/compose/smoke.sh leadscore:ci`: `docker compose up`, a webhook, and timer runs |

Install the linter with
`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0`. Do not silence a
finding inline. Fix it, or change `.golangci.yml` with a comment saying why.

After a dependency change, run `scripts/third-party-licenses.sh` to refresh
`third_party/licenses/`, then update the module list in `NOTICE`.

## Adding an adapter or a store

leadscore takes plug-ins: sources, enrichers, pollers, lookups, sinks, detectors and stores.
`docs/reference.md`, "Plug-ins", has the interfaces. `docs/postgres-store.md` walks through a
store.

1. Put the adapter in its own package under `adapters/` and register it in `init` with the
   matching `api.Register*` function. Inside this module only `cmd/` and `_test` files import
   the root package; everything else uses `internal/api`, and `imports_test.go` holds that. An
   adapter outside this repo uses `leadscore.Register*` instead.
2. Import a built-in adapter for its side effect in `cmd/leadscore/main.go`.
3. A sink passes `sinktest.Run`. Point the sink at a fake of its vendor that implements
   `sinktest.Vendor`, and list the destinations in `sinktest.Harness`. See
   `adapters/hubspot/sink_test.go`.
4. A store passes `storetest.Run`, from an external test package. See
   `internal/store/sqlite/conformance_test.go`.
5. Tests use a fake vendor, never a real key. A live check goes behind its own variable, as
   above.

## Pull requests

- **Tests:** every behaviour change comes with a test that fails before it and passes after.
- **Safety:** never contact an opted-out person, never cold-contact a person twice, never
  acknowledge a webhook before it is stored. A change near these carries its tests.
- **Logs:** logs carry ids, not emails. Pass log text through `internal/logredact`.
- **Changelog:** add a line under `Unreleased` in `CHANGELOG.md` for anything a user would
  notice. Tests, CI and docs changes need none.
- **Commits:** a short imperative subject (`Retry Sheets 429 with backoff`), and a body that
  says why when it is not obvious.

## Licensing of contributions

No sign-off line and no separately signed agreement are needed. By opening a pull request you
agree that your contribution is released under the project's [LICENSE](LICENSE).
